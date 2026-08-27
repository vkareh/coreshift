package certgen

import (
	"crypto/x509"
	"encoding/pem"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
)

func TestGenerateClientCertAndAdminKubeconfig(t *testing.T) {
	caCertPEM, caKeyPEM, err := GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := GenerateClientCert(caCertPEM, caKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	certBlock, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if cert.Subject.CommonName != "admin" || len(cert.Subject.Organization) != 1 || cert.Subject.Organization[0] != "system:masters" {
		t.Fatalf("unexpected client identity: %v", cert.Subject)
	}
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Fatal("generated certificate is not for client authentication")
	}
	caBlock, _ := pem.Decode(caCertPEM)
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := cert.CheckSignatureFrom(caCert); err != nil {
		t.Fatalf("certificate is not signed by CA: %v", err)
	}
	if _, err := x509.ParsePKCS8PrivateKey(pemDecode(t, keyPEM).Bytes); err != nil {
		t.Fatal(err)
	}

	kubeconfig, err := GenerateAdminKubeconfig(caCertPEM, caKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	config, err := clientcmd.Load(kubeconfig)
	if err != nil {
		t.Fatalf("load kubeconfig: %v", err)
	}
	cluster := config.Clusters["coreshift"]
	if cluster == nil || cluster.Server != "https://127.0.0.1:6443" || len(cluster.CertificateAuthorityData) == 0 {
		t.Fatalf("unexpected cluster config: %#v", cluster)
	}
	user := config.AuthInfos["admin"]
	if user == nil || len(user.ClientCertificateData) == 0 || len(user.ClientKeyData) == 0 {
		t.Fatalf("unexpected user config: %#v", user)
	}
	if config.CurrentContext != "admin@coreshift" {
		t.Fatalf("unexpected current context: %q", config.CurrentContext)
	}
}

func pemDecode(t *testing.T, data []byte) *pem.Block {
	t.Helper()
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatal("data is not PEM encoded")
	}
	return block
}
