package main

import (
	"bytes"
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

	"github.com/vkareh/coreshift/internal/certgen"
	"github.com/vkareh/coreshift/internal/oauth"
	clientcmd "k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestEnsureCertificatesReusesCompleteSet(t *testing.T) {
	stateDir := t.TempDir()
	if err := ensureCertificates(stateDir); err != nil {
		t.Fatal(err)
	}
	caBefore := readFile(t, stateDir, certgen.CACertFilename)

	if err := ensureCertificates(stateDir); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(caBefore, readFile(t, stateDir, certgen.CACertFilename)) {
		t.Fatal("complete certificate set was regenerated")
	}
}

func TestEnsureCertificatesRegeneratesSetWithoutLoopbackSAN(t *testing.T) {
	stateDir := t.TempDir()
	if err := ensureCertificates(stateDir); err != nil {
		t.Fatal(err)
	}
	writeStaleServiceServingCert(t, stateDir)

	if err := ensureCertificates(stateDir); err != nil {
		t.Fatal(err)
	}
	cert := parseCertificate(t, readFile(t, stateDir, certgen.ServiceServingCertFilename))
	if err := cert.VerifyHostname("127.0.0.1"); err != nil {
		t.Fatalf("regenerated certificate lacks loopback SAN: %v", err)
	}
}

// writeStaleServiceServingCert replaces the service-serving certificate with
// one covering only the service DNS names, as generated before the loopback
// address was added.
func writeStaleServiceServingCert(t *testing.T, stateDir string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "openshift-apiserver"},
		DNSNames:     []string{"openshift-apiserver", "openshift-oauth-apiserver"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(filepath.Join(stateDir, certgen.ServiceServingCertFilename), certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreAdminContextSwitchesBackFromLoginContext(t *testing.T) {
	stateDir := t.TempDir()
	if err := ensureCertificates(stateDir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(stateDir, certgen.AdminKubeconfigFilename)

	// Simulate an oc login: a token context is added and made current.
	config, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	loginContext := "dev/127-0-0-1:6443"
	config.AuthInfos[loginContext] = &clientcmdapi.AuthInfo{Token: "sha256~example"}
	config.Contexts[loginContext] = &clientcmdapi.Context{Cluster: "coreshift", AuthInfo: loginContext}
	config.CurrentContext = loginContext
	if err := clientcmd.WriteToFile(*config, path); err != nil {
		t.Fatal(err)
	}

	if err := restoreAdminContext(stateDir); err != nil {
		t.Fatal(err)
	}

	config, err = clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.CurrentContext != "admin@coreshift" {
		t.Fatalf("current context = %q, want admin@coreshift", config.CurrentContext)
	}
	if config.Contexts[loginContext] == nil || config.AuthInfos[loginContext] == nil {
		t.Fatal("login context was removed")
	}
}

func TestRestoreAdminContextKeepsCorrectSelectionUntouched(t *testing.T) {
	stateDir := t.TempDir()
	if err := ensureCertificates(stateDir); err != nil {
		t.Fatal(err)
	}

	before := readFile(t, stateDir, certgen.AdminKubeconfigFilename)
	if err := restoreAdminContext(stateDir); err != nil {
		t.Fatal(err)
	}
	if after := readFile(t, stateDir, certgen.AdminKubeconfigFilename); !bytes.Equal(before, after) {
		t.Fatal("kubeconfig was rewritten although the selection was already correct")
	}
}

func TestWriteOAuthServerConfigWithoutProviders(t *testing.T) {
	path, err := writeOAuthServerConfig(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	config := string(readFile(t, filepath.Dir(path), filepath.Base(path)))
	if !strings.Contains(config, "identityProviders: []") {
		t.Fatalf("config without providers is not deny-all only:\n%s", config)
	}
}

func TestWriteOAuthServerConfigRendersProviders(t *testing.T) {
	stateDir := t.TempDir()
	file := filepath.Join(stateDir, "htpasswd-0")
	providers := []oauth.Provider{{Name: "developers", MappingMethod: "claim", File: file}}

	path, err := writeOAuthServerConfig(stateDir, providers)
	if err != nil {
		t.Fatal(err)
	}
	config := string(readFile(t, filepath.Dir(path), filepath.Base(path)))
	want := `  identityProviders:
  - name: developers
    challenge: true
    login: true
    mappingMethod: claim
    provider:
      apiVersion: osin.config.openshift.io/v1
      kind: HTPasswdPasswordIdentityProvider
      file: ` + file + "\n"
	if !strings.Contains(config, want) {
		t.Fatalf("config missing provider stanza, want:\n%s\ngot:\n%s", want, config)
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
	block := pemDecode(t, certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func pemDecode(t *testing.T, data []byte) *pem.Block {
	t.Helper()
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatal("data is not PEM encoded")
	}
	return block
}
