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
}

const defaultAPIServerURL = "https://127.0.0.1:6443"

// Artifacts written by GenerateAll into the output directory.
const (
	CACertFilename                       = "ca.crt"
	CAKeyFilename                        = "ca.key"
	APIServerCertFilename                = "apiserver.crt"
	APIServerKeyFilename                 = "apiserver.key"
	FrontProxyCACertFilename             = "front-proxy-ca.crt"
	FrontProxyCAKeyFilename              = "front-proxy-ca.key"
	FrontProxyClientCertFilename         = "front-proxy-client.crt"
	FrontProxyClientKeyFilename          = "front-proxy-client.key"
	ServiceServingCertFilename           = "service-serving.crt"
	ServiceServingKeyFilename            = "service-serving.key"
	AdminCertFilename                    = "admin.crt"
	AdminKeyFilename                     = "admin.key"
	AdminKubeconfigFilename              = "admin.kubeconfig"
	ControllerManagerKubeconfigFilename  = "kube-controller-manager.kubeconfig"
	SchedulerKubeconfigFilename          = "kube-scheduler.kubeconfig"
	OpenShiftAPIServerKubeconfigFilename = "openshift-apiserver.kubeconfig"
	OAuthAPIServerKubeconfigFilename     = "oauth-apiserver.kubeconfig"
	SessionSecretsFilename               = "session-secrets.yaml"
	SAKeyFilename                        = "sa.key"
	SAPubFilename                        = "sa.pub"
)

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
	kubeconfig, err := generateKubeconfig(serverURL, "admin", caCertPEM, adminCertPEM, adminKeyPEM)
	if err != nil {
		return fmt.Errorf("generate admin kubeconfig: %w", err)
	}
	controllerManagerKubeconfig, err := generateComponentKubeconfig("system:kube-controller-manager", serverURL, caCertPEM, caKeyPEM)
	if err != nil {
		return fmt.Errorf("generate kube-controller-manager kubeconfig: %w", err)
	}
	schedulerKubeconfig, err := generateComponentKubeconfig("system:kube-scheduler", serverURL, caCertPEM, caKeyPEM)
	if err != nil {
		return fmt.Errorf("generate kube-scheduler kubeconfig: %w", err)
	}
	openshiftAPIServerKubeconfig, err := generateAggregatedKubeconfig(serverURL, caCertPEM, caKeyPEM)
	if err != nil {
		return fmt.Errorf("generate openshift-apiserver kubeconfig: %w", err)
	}
	oauthAPIServerKubeconfig, err := generateAggregatedKubeconfig(serverURL, caCertPEM, caKeyPEM)
	if err != nil {
		return fmt.Errorf("generate oauth-apiserver kubeconfig: %w", err)
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
