package worker

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	clientcmd "k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestReadNodeName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "node-name")
	if err := os.WriteFile(path, []byte("  worker1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	name, err := readNodeName(path)
	if err != nil {
		t.Fatal(err)
	}
	if name != "worker1" {
		t.Fatalf("name = %q, want worker1", name)
	}
}

func TestReadNodeNameRefusesMissingAndEmpty(t *testing.T) {
	if _, err := readNodeName(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("expected an error for a missing node name file")
	}
	path := filepath.Join(t.TempDir(), "node-name")
	if err := os.WriteFile(path, []byte("  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readNodeName(path); err == nil {
		t.Fatal("expected an error for an empty node name file")
	}
}

// selfSign creates a self-signed certificate for testing issued material.
func selfSign(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "system:node:worker1", Organization: []string{"system:nodes"}},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
	}, &x509.Certificate{PublicKey: &key.PublicKey}, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// approveOnCreate makes the fake client answer a certificate request
// creation with an approved and issued one, so the whole request path can
// run without a live approver.
func approveOnCreate(t *testing.T, client *fake.Clientset) {
	t.Helper()
	cert := selfSign(t)
	client.PrependReactor("create", "certificatesigningrequests", func(action k8stesting.Action) (bool, runtime.Object, error) {
		csr := action.(k8stesting.CreateAction).GetObject().(*certificatesv1.CertificateSigningRequest)
		csr.Status.Conditions = append(csr.Status.Conditions, certificatesv1.CertificateSigningRequestCondition{
			Type:   certificatesv1.CertificateApproved,
			Status: corev1.ConditionTrue,
		})
		csr.Status.Certificate = cert
		return false, csr, nil
	})
}

// TestRequestCertificate checks the certificate request the join sends:
// its name, signer, and usages, and the subject of the embedded request,
// which are what the machine approver recognizes as a node client.
func TestRequestCertificate(t *testing.T) {
	client := fake.NewSimpleClientset()
	approveOnCreate(t, client)

	certPEM, keyPEM, err := requestCertificate(context.Background(), client, "worker1")
	if err != nil {
		t.Fatal(err)
	}
	csr, err := client.CertificatesV1().CertificateSigningRequests().Get(
		context.Background(), "system:node:worker1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if csr.Spec.SignerName != certificatesv1.KubeAPIServerClientSignerName {
		t.Fatalf("signer = %q, want the kube-apiserver-client signer", csr.Spec.SignerName)
	}
	wantUsages := []certificatesv1.KeyUsage{
		certificatesv1.UsageDigitalSignature,
		certificatesv1.UsageKeyEncipherment,
		certificatesv1.UsageClientAuth,
	}
	if len(csr.Spec.Usages) != len(wantUsages) {
		t.Fatalf("usages = %v, want %v", csr.Spec.Usages, wantUsages)
	}
	for i, usage := range csr.Spec.Usages {
		if usage != wantUsages[i] {
			t.Fatalf("usages = %v, want %v", csr.Spec.Usages, wantUsages)
		}
	}

	block, _ := pem.Decode(csr.Spec.Request)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		t.Fatal("certificate request is not PEM encoded")
	}
	request, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if request.Subject.CommonName != "system:node:worker1" {
		t.Fatalf("common name = %q, want system:node:worker1", request.Subject.CommonName)
	}
	if len(request.Subject.Organization) != 1 || request.Subject.Organization[0] != "system:nodes" {
		t.Fatalf("organization = %v, want system:nodes", request.Subject.Organization)
	}

	pemEncodes(t, certPEM, "CERTIFICATE")
	key, err := x509.ParsePKCS8PrivateKey(pemEncodes(t, keyPEM, "PRIVATE KEY"))
	if err != nil {
		t.Fatalf("key is not PKCS 8: %v", err)
	}
	if _, ok := key.(*rsa.PrivateKey); !ok {
		t.Fatal("key is not an RSA key")
	}
}

// pemEncodes asserts that data holds a PEM block of the given type and
// returns the block's contents.
func pemEncodes(t *testing.T, data []byte, blockType string) []byte {
	t.Helper()
	block, _ := pem.Decode(data)
	if block == nil || block.Type != blockType {
		t.Fatalf("data is not a PEM %s block", blockType)
	}
	return block.Bytes
}

func TestRequestCertificateRefusesTakenName(t *testing.T) {
	client := fake.NewSimpleClientset(&certificatesv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "system:node:worker1"},
	})
	_, _, err := requestCertificate(context.Background(), client, "worker1")
	if !apierrors.IsAlreadyExists(err) {
		t.Fatalf("err = %v, want the taken name refused", err)
	}
}

func TestWaitIssuedReturnsIssuedCertificate(t *testing.T) {
	cert := selfSign(t)
	client := fake.NewSimpleClientset(&certificatesv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "system:node:worker1"},
		Status: certificatesv1.CertificateSigningRequestStatus{
			Conditions: []certificatesv1.CertificateSigningRequestCondition{{
				Type:   certificatesv1.CertificateApproved,
				Status: corev1.ConditionTrue,
			}},
			Certificate: cert,
		},
	})
	got, err := waitIssued(context.Background(), client, "system:node:worker1")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(cert) {
		t.Fatal("waitIssued did not return the issued certificate")
	}
}

// TestWaitIssuedWaitsForApproval checks that an approved but unissued
// request keeps being waited on rather than answered half-way.
func TestWaitIssuedWaitsForApproval(t *testing.T) {
	apiPollInterval = time.Millisecond
	t.Cleanup(func() { apiPollInterval = time.Second })
	client := fake.NewSimpleClientset(&certificatesv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "system:node:worker1"},
		Status: certificatesv1.CertificateSigningRequestStatus{
			Conditions: []certificatesv1.CertificateSigningRequestCondition{{
				Type:   certificatesv1.CertificateApproved,
				Status: corev1.ConditionTrue,
			}},
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := waitIssued(ctx, client, "system:node:worker1"); err == nil {
		t.Fatal("expected the wait to keep waiting for issuance")
	}
}

// TestWaitIssuedFailsRefusedRequest checks that a denied or failed
// request fails the wait instead of blocking forever.
func TestWaitIssuedFailsRefusedRequest(t *testing.T) {
	for _, refusalType := range []certificatesv1.RequestConditionType{
		certificatesv1.CertificateDenied,
		certificatesv1.CertificateFailed,
	} {
		client := fake.NewSimpleClientset(&certificatesv1.CertificateSigningRequest{
			ObjectMeta: metav1.ObjectMeta{Name: "system:node:worker1"},
			Status: certificatesv1.CertificateSigningRequestStatus{
				Conditions: []certificatesv1.CertificateSigningRequestCondition{{
					Type:   refusalType,
					Status: corev1.ConditionTrue,
					Reason: "by test",
				}},
			},
		})
		_, err := waitIssued(context.Background(), client, "system:node:worker1")
		want := strings.ToLower(string(refusalType))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: err = %v, want the refusal to fail the wait", refusalType, err)
		}
	}
}

// TestClusterInfo checks the CA and server the join takes from the
// bootstrap kubeconfig: embedded CA data, a referenced CA file, and the
// refusals for a kubeconfig carrying neither.
func TestClusterInfo(t *testing.T) {
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	caPEM := selfSign(t)
	if err := os.WriteFile(caFile, caPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, cluster := range map[string]*clientcmdapi.Cluster{
		"embedded CA": {Server: "https://127.0.0.1:6443", CertificateAuthorityData: caPEM},
		"referenced CA": {
			Server:               "https://127.0.0.1:6443",
			CertificateAuthority: caFile,
		},
	} {
		config := clientcmdapi.NewConfig()
		config.Clusters["coreshift"] = cluster
		config.Contexts["bootstrap@coreshift"] = &clientcmdapi.Context{Cluster: "coreshift", AuthInfo: "bootstrap"}
		config.AuthInfos["bootstrap"] = &clientcmdapi.AuthInfo{Token: "token"}
		config.CurrentContext = "bootstrap@coreshift"

		server, got, err := clusterInfo(config)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if server != "https://127.0.0.1:6443" {
			t.Fatalf("%s: server = %q", name, server)
		}
		if string(got) != string(caPEM) {
			t.Fatalf("%s: CA does not match the kubeconfig's", name)
		}
	}

	config := clientcmdapi.NewConfig()
	config.Clusters["coreshift"] = &clientcmdapi.Cluster{Server: "https://127.0.0.1:6443"}
	if _, _, err := clusterInfo(config); err == nil {
		t.Fatal("expected a refusal for a kubeconfig carrying no CA")
	}
}

// TestWriteNodeKubeconfig checks the assembled node kubeconfig: the
// cluster the bootstrap kubeconfig pointed at, the node's certificate and
// key as its credentials, and the node context selected.
func TestWriteNodeKubeconfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	caPEM, certPEM := selfSign(t), selfSign(t)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("key")})

	if err := writeNodeKubeconfig(path, "https://127.0.0.1:6443", "worker1", caPEM, certPEM, keyPEM); err != nil {
		t.Fatal(err)
	}
	config, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cluster := config.Clusters["coreshift"]
	if cluster == nil || cluster.Server != "https://127.0.0.1:6443" {
		t.Fatalf("cluster = %v, want the bootstrap cluster", cluster)
	}
	if string(cluster.CertificateAuthorityData) != string(caPEM) {
		t.Fatal("kubeconfig does not carry the control plane CA")
	}
	auth := config.AuthInfos["system:node:worker1"]
	if auth == nil || string(auth.ClientCertificateData) != string(certPEM) || string(auth.ClientKeyData) != string(keyPEM) {
		t.Fatalf("auth info = %v, want the node's certificate and key", auth)
	}
	if config.CurrentContext != "system:node:worker1@coreshift" {
		t.Fatalf("current context = %q, want the node's", config.CurrentContext)
	}
}
