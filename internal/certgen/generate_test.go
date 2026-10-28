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

	certs := []string{CACertFilename, APIServerCertFilename, FrontProxyCACertFilename, FrontProxyClientCertFilename, ServiceServingCertFilename, AdminCertFilename, SAPubFilename}
	keys := []string{CAKeyFilename, APIServerKeyFilename, FrontProxyCAKeyFilename, FrontProxyClientKeyFilename, ServiceServingKeyFilename, AdminKeyFilename, SAKeyFilename}
	kubeconfigs := []string{AdminKubeconfigFilename, ControllerManagerKubeconfigFilename, SchedulerKubeconfigFilename, OpenShiftAPIServerKubeconfigFilename, OAuthAPIServerKubeconfigFilename, OAuthServerKubeconfigFilename, OpenShiftControllerManagerKubeconfigFilename, ClusterPolicyControllerKubeconfigFilename, ClusterMachineApproverKubeconfigFilename}
	for _, name := range append(append(certs, keys...), kubeconfigs...) {
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
	frontProxyCACert := parseCertificate(t, readFile(t, outputDir, FrontProxyCACertFilename))
	frontProxyClientCert := parseCertificate(t, readFile(t, outputDir, FrontProxyClientCertFilename))
	if err := frontProxyClientCert.CheckSignatureFrom(frontProxyCACert); err != nil {
		t.Fatalf("front-proxy client certificate is not signed by front-proxy CA: %v", err)
	}
	serviceServingCert := parseCertificate(t, readFile(t, outputDir, ServiceServingCertFilename))
	if err := serviceServingCert.CheckSignatureFrom(caCert); err != nil {
		t.Fatalf("service-serving certificate is not signed by CA: %v", err)
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

	// The components Hypershift runs with its admin kubeconfig use the
	// system:admin identity, so their embedded client certificates must
	// carry the system:masters group.
	for _, name := range []string{OpenShiftAPIServerKubeconfigFilename, OAuthAPIServerKubeconfigFilename, OAuthServerKubeconfigFilename, OpenShiftControllerManagerKubeconfigFilename, ClusterPolicyControllerKubeconfigFilename} {
		config, err := clientcmd.Load(readFile(t, outputDir, name))
		if err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
		user, ok := config.AuthInfos["system:admin"]
		if !ok {
			t.Fatalf("%s has no system:admin user", name)
		}
		cert := parseCertificate(t, user.ClientCertificateData)
		if cert.Subject.CommonName != "system:admin" {
			t.Fatalf("%s client certificate common name = %q, want system:admin", name, cert.Subject.CommonName)
		}
		if err := cert.CheckSignatureFrom(caCert); err != nil {
			t.Fatalf("%s client certificate is not signed by CA: %v", name, err)
		}
		found := false
		for _, org := range cert.Subject.Organization {
			found = found || org == "system:masters"
		}
		if !found {
			t.Fatalf("%s client certificate is not in the system:masters group", name)
		}
	}
	// The machine approver runs with its own controller identity, so its
	// kubeconfig's certificate carries that name and no group.
	config, err = clientcmd.Load(readFile(t, outputDir, ClusterMachineApproverKubeconfigFilename))
	if err != nil {
		t.Fatalf("load %s: %v", ClusterMachineApproverKubeconfigFilename, err)
	}
	user, ok := config.AuthInfos["system:openshift:controller:machine-approver"]
	if !ok {
		t.Fatalf("%s has no machine approver user", ClusterMachineApproverKubeconfigFilename)
	}
	cert := parseCertificate(t, user.ClientCertificateData)
	if cert.Subject.CommonName != "system:openshift:controller:machine-approver" {
		t.Fatalf("%s client certificate common name = %q", ClusterMachineApproverKubeconfigFilename, cert.Subject.CommonName)
	}
	if len(cert.Subject.Organization) != 0 {
		t.Fatalf("%s client certificate carries groups: %v", ClusterMachineApproverKubeconfigFilename, cert.Subject.Organization)
	}
	if err := cert.CheckSignatureFrom(caCert); err != nil {
		t.Fatalf("%s client certificate is not signed by CA: %v", ClusterMachineApproverKubeconfigFilename, err)
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

	for _, name := range []string{ControllerManagerKubeconfigFilename, SchedulerKubeconfigFilename, OpenShiftAPIServerKubeconfigFilename, OAuthAPIServerKubeconfigFilename, OAuthServerKubeconfigFilename, OpenShiftControllerManagerKubeconfigFilename, ClusterPolicyControllerKubeconfigFilename, ClusterMachineApproverKubeconfigFilename} {
		config, err := clientcmd.Load(readFile(t, outputDir, name))
		if err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
		if cluster := config.Clusters["coreshift"]; cluster == nil || cluster.Server != "https://coreshift.example.com:6443" {
			t.Fatalf("unexpected cluster config in %s: %#v", name, cluster)
		}
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

	for _, name := range []string{CAKeyFilename, APIServerKeyFilename, FrontProxyCAKeyFilename, FrontProxyClientKeyFilename, ServiceServingKeyFilename, AdminKeyFilename, AdminKubeconfigFilename, ControllerManagerKubeconfigFilename, SchedulerKubeconfigFilename, OpenShiftAPIServerKubeconfigFilename, OAuthAPIServerKubeconfigFilename, OAuthServerKubeconfigFilename, OpenShiftControllerManagerKubeconfigFilename, ClusterPolicyControllerKubeconfigFilename, ClusterMachineApproverKubeconfigFilename, SAKeyFilename} {
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
