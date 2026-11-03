package certgen

import (
	"fmt"
	"os"
	"path/filepath"
)

// Options controls how certificates and the admin kubeconfig are generated.
// The zero value is valid and uses defaults for every field.
type Options struct {
	// APIServerURL is the API server endpoint referenced by the admin
	// kubeconfig. Defaults to https://127.0.0.1:6443.
	APIServerURL string
	// ClusterName is the name the kubeconfig entries — cluster, context,
	// and user — carry, so the kubeconfig files of several clusters merge
	// without colliding. Defaults to coreshift.
	ClusterName string
}

const (
	defaultAPIServerURL = "https://127.0.0.1:6443"
	defaultClusterName  = "coreshift"
)

// Artifacts written by GenerateAll into the output directory.
const (
	CACertFilename                               = "ca.crt"
	CAKeyFilename                                = "ca.key"
	APIServerCertFilename                        = "apiserver.crt"
	APIServerKeyFilename                         = "apiserver.key"
	FrontProxyCACertFilename                     = "front-proxy-ca.crt"
	FrontProxyCAKeyFilename                      = "front-proxy-ca.key"
	FrontProxyClientCertFilename                 = "front-proxy-client.crt"
	FrontProxyClientKeyFilename                  = "front-proxy-client.key"
	ServiceServingCertFilename                   = "service-serving.crt"
	ServiceServingKeyFilename                    = "service-serving.key"
	AdminCertFilename                            = "admin.crt"
	AdminKeyFilename                             = "admin.key"
	AdminKubeconfigFilename                      = "admin.kubeconfig"
	ControllerManagerKubeconfigFilename          = "kube-controller-manager.kubeconfig"
	SchedulerKubeconfigFilename                  = "kube-scheduler.kubeconfig"
	OpenShiftAPIServerKubeconfigFilename         = "openshift-apiserver.kubeconfig"
	OAuthAPIServerKubeconfigFilename             = "oauth-apiserver.kubeconfig"
	OAuthServerKubeconfigFilename                = "oauth-server.kubeconfig"
	OpenShiftControllerManagerKubeconfigFilename         = "openshift-controller-manager.kubeconfig"
	ClusterPolicyControllerKubeconfigFilename            = "cluster-policy-controller.kubeconfig"
	ClusterMachineApproverKubeconfigFilename             = "cluster-machine-approver.kubeconfig"
	ClusterNetworkOperatorKubeconfigFilename             = "cluster-network-operator.kubeconfig"
	SessionSecretsFilename                       = "session-secrets.yaml"
	SAKeyFilename                                = "sa.key"
	SAPubFilename                                = "sa.pub"
)

// machineApproverIdentity is the username the machine approver's client
// certificate carries: the service account the release payload's own
// binding grants, so the approver needs no permissions seeded for it.
// A certificate common name stands in for the account's token the way
// the other component identities stand in for theirs.
const machineApproverIdentity = "system:serviceaccount:openshift-cluster-machine-approver:machine-approver-sa"

// networkOperatorIdentity is the username the cluster network
// operator's client certificate carries: the service account the
// release payload's own cluster-admin binding grants, so the operator
// needs no permissions seeded for it.
const networkOperatorIdentity = "system:serviceaccount:openshift-network-operator:cluster-network-operator"

// GenerateAll generates every certificate and key needed to run a CoreShift
// control plane, plus client kubeconfigs, writing all of them to outputDir.
// The on-disk layout is:
//
//	ca.crt, ca.key                                  self-signed certificate authority
//	apiserver.crt, apiserver.key                    kube-apiserver serving certificate
//	front-proxy-ca.crt, front-proxy-ca.key          CA trusted by aggregated API servers
//	front-proxy-client.crt, front-proxy-client.key  kube-apiserver certificate for aggregated API servers
//	service-serving.crt, service-serving.key        serving certificate for the OpenShift API server and OAuth API server
//	admin.crt, admin.key                            cluster-admin client certificate
//	admin.kubeconfig                                kubeconfig for the admin client
//	kube-controller-manager.kubeconfig              kubeconfig for kube-controller-manager
//	kube-scheduler.kubeconfig                       kubeconfig for kube-scheduler
//	openshift-apiserver.kubeconfig                  kubeconfig for the OpenShift API server
//	oauth-apiserver.kubeconfig                      kubeconfig for the OAuth API server
//	oauth-server.kubeconfig                         kubeconfig for the OAuth server
//	openshift-controller-manager.kubeconfig          kubeconfig for the OpenShift controller manager
//	cluster-policy-controller.kubeconfig            kubeconfig for the cluster policy controller
//	cluster-machine-approver.kubeconfig              kubeconfig for the cluster machine approver
//	cluster-network-operator.kubeconfig              kubeconfig for the cluster network operator
//	session-secrets.yaml                            session signing and encryption secrets for the OAuth server
//	sa.key, sa.pub                                  service-account signing key pair
//
// Each kubeconfig embeds the CA and its client's certificate material, so it
// is usable directly regardless of the output directory's location. outputDir
// is created if it does not exist.
func GenerateAll(outputDir string, opts Options) error {
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}

	caCertPEM, caKeyPEM, err := GenerateCA()
	if err != nil {
		return fmt.Errorf("generate CA: %w", err)
	}
	apiserverCertPEM, apiserverKeyPEM, err := GenerateAPIServerCert(caCertPEM, caKeyPEM)
	if err != nil {
		return fmt.Errorf("generate API server certificate: %w", err)
	}
	frontProxyCACertPEM, frontProxyCAKeyPEM, err := GenerateFrontProxyCA()
	if err != nil {
		return fmt.Errorf("generate front-proxy CA: %w", err)
	}
	frontProxyClientCertPEM, frontProxyClientKeyPEM, err := GenerateFrontProxyClientCert(frontProxyCACertPEM, frontProxyCAKeyPEM)
	if err != nil {
		return fmt.Errorf("generate front-proxy client certificate: %w", err)
	}
	serviceServingCertPEM, serviceServingKeyPEM, err := GenerateServiceServingCert(caCertPEM, caKeyPEM)
	if err != nil {
		return fmt.Errorf("generate service-serving certificate: %w", err)
	}
	adminCertPEM, adminKeyPEM, err := GenerateClientCert(caCertPEM, caKeyPEM)
	if err != nil {
		return fmt.Errorf("generate admin client certificate: %w", err)
	}
	saKeyPEM, saPubPEM, err := GenerateServiceAccountKeyPair()
	if err != nil {
		return fmt.Errorf("generate service-account key pair: %w", err)
	}
	sessionSecrets, err := GenerateSessionSecrets()
	if err != nil {
		return fmt.Errorf("generate session secrets: %w", err)
	}

	serverURL := opts.APIServerURL
	if serverURL == "" {
		serverURL = defaultAPIServerURL
	}
	clusterName := opts.ClusterName
	if clusterName == "" {
		clusterName = defaultClusterName
	}
	kubeconfig, err := generateKubeconfig(serverURL, "admin", clusterName, caCertPEM, adminCertPEM, adminKeyPEM)
	if err != nil {
		return fmt.Errorf("generate admin kubeconfig: %w", err)
	}
	controllerManagerKubeconfig, err := generateComponentKubeconfig("system:kube-controller-manager", serverURL, clusterName, caCertPEM, caKeyPEM)
	if err != nil {
		return fmt.Errorf("generate kube-controller-manager kubeconfig: %w", err)
	}
	schedulerKubeconfig, err := generateComponentKubeconfig("system:kube-scheduler", serverURL, clusterName, caCertPEM, caKeyPEM)
	if err != nil {
		return fmt.Errorf("generate kube-scheduler kubeconfig: %w", err)
	}
	openshiftAPIServerKubeconfig, err := generateAdminKubeconfig(serverURL, clusterName, caCertPEM, caKeyPEM)
	if err != nil {
		return fmt.Errorf("generate openshift-apiserver kubeconfig: %w", err)
	}
	oauthAPIServerKubeconfig, err := generateAdminKubeconfig(serverURL, clusterName, caCertPEM, caKeyPEM)
	if err != nil {
		return fmt.Errorf("generate oauth-apiserver kubeconfig: %w", err)
	}
	oauthServerKubeconfig, err := generateAdminKubeconfig(serverURL, clusterName, caCertPEM, caKeyPEM)
	if err != nil {
		return fmt.Errorf("generate oauth-server kubeconfig: %w", err)
	}
	openShiftControllerManagerKubeconfig, err := generateAdminKubeconfig(serverURL, clusterName, caCertPEM, caKeyPEM)
	if err != nil {
		return fmt.Errorf("generate openshift-controller-manager kubeconfig: %w", err)
	}
	clusterPolicyControllerKubeconfig, err := generateAdminKubeconfig(serverURL, clusterName, caCertPEM, caKeyPEM)
	if err != nil {
		return fmt.Errorf("generate cluster-policy-controller kubeconfig: %w", err)
	}
	// The machine approver runs with the payload's own service-account
	// identity rather than the admin identity, the way its deployment
	// does in real clusters; the version operator delivers the role and
	// binding that grant it, so nothing is seeded for it here.
	clusterMachineApproverKubeconfig, err := generateComponentKubeconfig(machineApproverIdentity, serverURL, clusterName, caCertPEM, caKeyPEM)
	if err != nil {
		return fmt.Errorf("generate cluster-machine-approver kubeconfig: %w", err)
	}
	// The network operator runs with the payload's own service-account
	// identity for the same reason: the cluster-admin binding the
	// version operator delivers is the operator's permission set.
	clusterNetworkOperatorKubeconfig, err := generateComponentKubeconfig(networkOperatorIdentity, serverURL, clusterName, caCertPEM, caKeyPEM)
	if err != nil {
		return fmt.Errorf("generate cluster-network-operator kubeconfig: %w", err)
	}

	files := []struct {
		name string
		data []byte
		mode os.FileMode
	}{
		{CACertFilename, caCertPEM, 0o644},
		{CAKeyFilename, caKeyPEM, 0o600},
		{APIServerCertFilename, apiserverCertPEM, 0o644},
		{APIServerKeyFilename, apiserverKeyPEM, 0o600},
		{FrontProxyCACertFilename, frontProxyCACertPEM, 0o644},
		{FrontProxyCAKeyFilename, frontProxyCAKeyPEM, 0o600},
		{FrontProxyClientCertFilename, frontProxyClientCertPEM, 0o644},
		{FrontProxyClientKeyFilename, frontProxyClientKeyPEM, 0o600},
		{ServiceServingCertFilename, serviceServingCertPEM, 0o644},
		{ServiceServingKeyFilename, serviceServingKeyPEM, 0o600},
		{AdminCertFilename, adminCertPEM, 0o644},
		{AdminKeyFilename, adminKeyPEM, 0o600},
		{AdminKubeconfigFilename, kubeconfig, 0o600},
		{ControllerManagerKubeconfigFilename, controllerManagerKubeconfig, 0o600},
		{SchedulerKubeconfigFilename, schedulerKubeconfig, 0o600},
		{OpenShiftAPIServerKubeconfigFilename, openshiftAPIServerKubeconfig, 0o600},
		{OAuthAPIServerKubeconfigFilename, oauthAPIServerKubeconfig, 0o600},
		{OAuthServerKubeconfigFilename, oauthServerKubeconfig, 0o600},
		{OpenShiftControllerManagerKubeconfigFilename, openShiftControllerManagerKubeconfig, 0o600},
		{ClusterPolicyControllerKubeconfigFilename, clusterPolicyControllerKubeconfig, 0o600},
		{ClusterMachineApproverKubeconfigFilename, clusterMachineApproverKubeconfig, 0o600},
		{ClusterNetworkOperatorKubeconfigFilename, clusterNetworkOperatorKubeconfig, 0o600},
		{SessionSecretsFilename, sessionSecrets, 0o600},
		{SAKeyFilename, saKeyPEM, 0o600},
		{SAPubFilename, saPubPEM, 0o644},
	}
	for _, f := range files {
		path := filepath.Join(outputDir, f.name)
		if err := os.WriteFile(path, f.data, f.mode); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}
