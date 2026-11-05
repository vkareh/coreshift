package worker

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// apiPollInterval spaces the polls while waiting on the API. A variable
// for test override.
var apiPollInterval = time.Second

// readNodeName returns the node name the host-side create command hands
// in.
func readNodeName(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read node name: %w", err)
	}
	name := strings.TrimSpace(string(data))
	if name == "" {
		return "", fmt.Errorf("node name file %s is empty", path)
	}
	return name, nil
}

// nodeUser is the node's client identity: the name every node client
// certificate carries, which the Node authorizer matches to the node's
// own objects.
func nodeUser(nodeName string) string {
	return "system:node:" + nodeName
}

// NodeUser is the name a worker's node client certificate carries, and
// the name of the certificate request the join makes for it; the
// host-side commands use it to clean up a worker's join residue.
func NodeUser(nodeName string) string {
	return nodeUser(nodeName)
}

// clusterInfo returns the API server address and CA certificate the
// bootstrap kubeconfig points at, from the CA the kubeconfig embeds or
// the file it references.
func clusterInfo(config *clientcmdapi.Config) (server string, caPEM []byte, err error) {
	current, ok := config.Contexts[config.CurrentContext]
	if !ok || current == nil {
		return "", nil, fmt.Errorf("bootstrap kubeconfig has no current context")
	}
	cluster := config.Clusters[current.Cluster]
	if cluster == nil {
		return "", nil, fmt.Errorf("bootstrap kubeconfig has no %s cluster", current.Cluster)
	}
	if len(cluster.CertificateAuthorityData) > 0 {
		return cluster.Server, cluster.CertificateAuthorityData, nil
	}
	if cluster.CertificateAuthority != "" {
		caPEM, err := os.ReadFile(cluster.CertificateAuthority)
		if err != nil {
			return "", nil, fmt.Errorf("read CA certificate: %w", err)
		}
		return cluster.Server, caPEM, nil
	}
	return "", nil, fmt.Errorf("bootstrap kubeconfig carries no CA certificate")
}

// requestCertificate requests the node's client certificate through the
// bootstrap identity and waits until it is approved and issued, returning
// the issued certificate and the key it belongs to.
func requestCertificate(ctx context.Context, client kubernetes.Interface, nodeName string) (certPEM, keyPEM []byte, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("generate node key: %w", err)
	}
	request, err := certificateRequest(nodeName, key)
	if err != nil {
		return nil, nil, err
	}
	csr := &certificatesv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{Name: nodeUser(nodeName)},
		Spec: certificatesv1.CertificateSigningRequestSpec{
			Request: request,
			Usages: []certificatesv1.KeyUsage{
				certificatesv1.UsageDigitalSignature,
				certificatesv1.UsageKeyEncipherment,
				certificatesv1.UsageClientAuth,
			},
			// The signer kubelet's own bootstrap requests node client
			// certificates from, and the one the machine approver approves
			// requests for.
			SignerName: certificatesv1.KubeAPIServerClientKubeletSignerName,
		},
	}
	// The name is deterministic, so a stale request from a previous
	// worker of this name must be gone before the join runs; the
	// host-side minting removes it, because the bootstrap identity may
	// only create requests.
	fmt.Println("Requesting node certificate")
	if _, err := client.CertificatesV1().CertificateSigningRequests().Create(ctx, csr, metav1.CreateOptions{}); err != nil {
		return nil, nil, fmt.Errorf("request node certificate: %w", err)
	}
	fmt.Println("Waiting for the node certificate to be approved")
	cert, err := waitIssued(ctx, client, csr.Name)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return cert, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

// certificateRequest builds the PEM certificate request for the node's
// client identity: the subject kubelet's own bootstrap sends, which the
// machine approver recognizes as a node client request.
func certificateRequest(nodeName string, key *rsa.PrivateKey) ([]byte, error) {
	request, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName:   nodeUser(nodeName),
			Organization: []string{"system:nodes"},
		},
	}, key)
	if err != nil {
		return nil, fmt.Errorf("create certificate request: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: request}), nil
}

// waitIssued polls the certificate request until it is approved and
// issued, returning the issued certificate. Everything but a denial or a
// signing failure is waited out: the request may sit unapproved until an
// operator or the machine approver acts on it.
func waitIssued(ctx context.Context, client kubernetes.Interface, name string) ([]byte, error) {
	err := wait.PollUntilContextCancel(ctx, apiPollInterval, true, func(ctx context.Context) (bool, error) {
		csr, err := client.CertificatesV1().CertificateSigningRequests().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			// The API may not be reachable yet; keep waiting.
			return false, nil
		}
		if failure := refusal(csr); failure != nil {
			return false, failure
		}
		return approved(csr) && len(csr.Status.Certificate) > 0, nil
	})
	if err != nil {
		return nil, fmt.Errorf("wait for the node certificate: %w", err)
	}
	csr, err := client.CertificatesV1().CertificateSigningRequests().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("read issued node certificate: %w", err)
	}
	return csr.Status.Certificate, nil
}

// refusal reports the error for a certificate request that was denied or
// failed to sign, so a refused join fails instead of waiting forever.
func refusal(csr *certificatesv1.CertificateSigningRequest) error {
	for _, condition := range csr.Status.Conditions {
		if condition.Status != corev1.ConditionTrue {
			continue
		}
		if condition.Type == certificatesv1.CertificateDenied || condition.Type == certificatesv1.CertificateFailed {
			return fmt.Errorf("node certificate request %s was %s: %s", csr.Name, strings.ToLower(string(condition.Type)), condition.Reason)
		}
	}
	return nil
}

// approved reports whether the certificate request carries a true
// approval.
func approved(csr *certificatesv1.CertificateSigningRequest) bool {
	for _, condition := range csr.Status.Conditions {
		if condition.Type == certificatesv1.CertificateApproved && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// writeNodeKubeconfig assembles the node kubeconfig from the issued
// certificate: the same cluster the bootstrap kubeconfig points at, with
// the node's client certificate in place of the bootstrap token. The
// certificate, key, and CA are embedded, so the one file carries the
// whole identity.
func writeNodeKubeconfig(path, server, nodeName string, caPEM, certPEM, keyPEM []byte) error {
	user := nodeUser(nodeName)
	config := clientcmdapi.NewConfig()
	config.Clusters["coreshift"] = &clientcmdapi.Cluster{
		Server:                   server,
		CertificateAuthorityData: caPEM,
	}
	config.AuthInfos[user] = &clientcmdapi.AuthInfo{
		ClientCertificateData: certPEM,
		ClientKeyData:         keyPEM,
	}
	config.Contexts[user+"@coreshift"] = &clientcmdapi.Context{
		Cluster:  "coreshift",
		AuthInfo: user,
	}
	config.CurrentContext = user + "@coreshift"
	if err := clientcmd.WriteToFile(*config, path); err != nil {
		return fmt.Errorf("write node kubeconfig: %w", err)
	}
	return nil
}

// The cluster DNS the kubelet hands every pod: the DNS Service the DNS
// operator reconciles, in the namespace its first reconcile creates,
// and the operator's default cluster domain.
const (
	dnsServiceNamespace = "openshift-dns"
	dnsServiceName      = "dns-default"
	clusterDomain       = "cluster.local"
)

// dnsReadyTimeout bounds the wait for the cluster's DNS. The DNS
// operator starts with the control plane and workers join after, so the
// wait usually passes at once; it covers the worker that joins while
// the operator's first reconcile is still in flight.
const dnsReadyTimeout = 5 * time.Minute

// resolveClusterDNS returns the cluster DNS address the kubelet hands
// every pod: the cluster IP of the DNS Service. The DNS operator starts
// with the control plane and the worker joins after, so a cluster whose
// DNS is not reconciled yet is waited for here, visibly and bounded,
// the way the control plane's startup gates wait for what the version
// operator delivers.
func resolveClusterDNS(ctx context.Context) (string, error) {
	config, err := clientcmd.BuildConfigFromFlags("", nodeKubeconfigPath)
	if err != nil {
		return "", fmt.Errorf("read node kubeconfig: %w", err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return "", fmt.Errorf("create node client: %w", err)
	}
	return waitClusterDNS(ctx, client)
}

// waitClusterDNS polls the DNS Service until it carries a cluster IP.
// The node identity the join just took may read services, the grant
// every node carries for its service lister.
func waitClusterDNS(ctx context.Context, client kubernetes.Interface) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, dnsReadyTimeout)
	defer cancel()
	announced := false
	var ip string
	pollErr := wait.PollUntilContextCancel(ctx, apiPollInterval, true, func(ctx context.Context) (bool, error) {
		service, err := client.CoreV1().Services(dnsServiceNamespace).Get(ctx, dnsServiceName, metav1.GetOptions{})
		if err == nil {
			ip = service.Spec.ClusterIP
		}
		if ip != "" {
			return true, nil
		}
		if !announced {
			announced = true
			fmt.Println("Waiting for the cluster's DNS")
		}
		return false, nil
	})
	if pollErr != nil {
		if errors.Is(pollErr, context.Canceled) {
			// Shutdown was requested mid-wait; the caller reports that.
			return "", pollErr
		}
		return "", fmt.Errorf("the DNS Service %s in namespace %s never arrived within %s; the DNS operator delivers it",
			dnsServiceName, dnsServiceNamespace, dnsReadyTimeout)
	}
	return ip, nil
}
