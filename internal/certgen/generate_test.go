package certgen

import (
	"bytes"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
)

func TestGenerateAll(t *testing.T) {
	outputDir := t.TempDir()

	if err := GenerateAll(outputDir, Options{}); err != nil {
		t.Fatal(err)
	}

	certs := []string{CACertFilename, APIServerCertFilename, AdminCertFilename, SAPubFilename}
	keys := []string{CAKeyFilename, APIServerKeyFilename, AdminKeyFilename, SAKeyFilename}
	for _, name := range append(append(certs, keys...), AdminKubeconfigFilename) {
		if _, err := os.Stat(filepath.Join(outputDir, name)); err != nil {
			t.Fatalf("expected %s to be written: %v", name, err)
		}
	}

	caCertPEM := readFile(t, outputDir, CACertFilename)
	adminCertPEM := readFile(t, outputDir, AdminCertFilename)
	caCert := parseCertificate(t, caCertPEM)
	adminCert := parseCertificate(t, readFile(t, outputDir, AdminCertFilename))
	if err := adminCert.CheckSignatureFrom(caCert); err != nil {
		t.Fatalf("admin certificate is not signed by CA: %v", err)
	}

	config, err := clientcmd.Load(readFile(t, outputDir, AdminKubeconfigFilename))
	if err != nil {
		t.Fatalf("load kubeconfig: %v", err)
	}
	cluster := config.Clusters["coreshift"]
	if cluster == nil || cluster.Server != defaultAPIServerURL {
		t.Fatalf("unexpected cluster config: %#v", cluster)
	}
	if !bytes.Equal(cluster.CertificateAuthorityData, caCertPEM) {
		t.Fatal("kubeconfig CA data does not match ca.crt")
	}
	user := config.AuthInfos["admin"]
	if user == nil || !bytes.Equal(user.ClientCertificateData, adminCertPEM) {
		t.Fatalf("unexpected user config: %#v", user)
	}
	if config.CurrentContext != "admin@coreshift" {
		t.Fatalf("unexpected current context: %q", config.CurrentContext)
	}
}

func TestGenerateAllCustomAPIServerURL(t *testing.T) {
	outputDir := t.TempDir()

	if err := GenerateAll(outputDir, Options{APIServerURL: "https://coreshift.example.com:6443"}); err != nil {
		t.Fatal(err)
	}

	config, err := clientcmd.Load(readFile(t, outputDir, AdminKubeconfigFilename))
	if err != nil {
		t.Fatalf("load kubeconfig: %v", err)
	}
	if cluster := config.Clusters["coreshift"]; cluster == nil || cluster.Server != "https://coreshift.example.com:6443" {
		t.Fatalf("unexpected cluster config: %#v", cluster)
	}
}

func TestGenerateAllCreatesNestedOutputDir(t *testing.T) {
	outputDir := filepath.Join(t.TempDir(), "state", "coreshift")

	if err := GenerateAll(outputDir, Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outputDir, CACertFilename)); err != nil {
		t.Fatalf("expected output directory to be created: %v", err)
	}
}

func TestGenerateAllKeyPermissions(t *testing.T) {
	outputDir := t.TempDir()

	if err := GenerateAll(outputDir, Options{}); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{CAKeyFilename, APIServerKeyFilename, AdminKeyFilename, AdminKubeconfigFilename, SAKeyFilename} {
		info, err := os.Stat(filepath.Join(outputDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("%s has permissions %o, want 0600", name, perm)
		}
	}
}

func readFile(t *testing.T, dir, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func parseCertificate(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	certBlock := pemDecode(t, certPEM)
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}
