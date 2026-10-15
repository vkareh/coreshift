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
	CACertFilename          = "ca.crt"
	CAKeyFilename           = "ca.key"
	APIServerCertFilename   = "apiserver.crt"
	APIServerKeyFilename    = "apiserver.key"
	AdminCertFilename       = "admin.crt"
	AdminKeyFilename        = "admin.key"
	AdminKubeconfigFilename = "admin.kubeconfig"
	SAKeyFilename           = "sa.key"
	SAPubFilename           = "sa.pub"
)

// GenerateAll generates every certificate and key needed to run a CoreShift
// control plane, plus an admin kubeconfig, writing all of them to outputDir.
// The on-disk layout is:
//
//	ca.crt, ca.key               self-signed certificate authority
//	apiserver.crt, apiserver.key kube-apiserver serving certificate
//	admin.crt, admin.key         cluster-admin client certificate
//	admin.kubeconfig             kubeconfig for the admin client
//	sa.key, sa.pub               service-account signing key pair
//
// The kubeconfig embeds the CA and admin certificate material, so it is
// usable directly regardless of the output directory's location. outputDir
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
	adminCertPEM, adminKeyPEM, err := GenerateClientCert(caCertPEM, caKeyPEM)
	if err != nil {
		return fmt.Errorf("generate admin client certificate: %w", err)
	}
	saKeyPEM, saPubPEM, err := GenerateServiceAccountKeyPair()
	if err != nil {
		return fmt.Errorf("generate service-account key pair: %w", err)
	}

	serverURL := opts.APIServerURL
	if serverURL == "" {
		serverURL = defaultAPIServerURL
	}
	kubeconfig, err := generateKubeconfig(serverURL, caCertPEM, adminCertPEM, adminKeyPEM)
	if err != nil {
		return fmt.Errorf("generate admin kubeconfig: %w", err)
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
		{AdminCertFilename, adminCertPEM, 0o644},
		{AdminKeyFilename, adminKeyPEM, 0o600},
		{AdminKubeconfigFilename, kubeconfig, 0o600},
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
