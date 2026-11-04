package certgen

import (
	"crypto/x509"
	"testing"
)

func TestGenerateKubeletClientCert(t *testing.T) {
	caCertPEM, caKeyPEM, err := GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := GenerateKubeletClientCert(caCertPEM, caKeyPEM)
	if err != nil {
		t.Fatal(err)
	}

	cert := parseCertificate(t, certPEM)
	if cert.Subject.CommonName != "system:kube-apiserver-kubelet-client" {
		t.Fatalf("unexpected client identity: %v", cert.Subject)
	}
	if len(cert.Subject.Organization) != 1 || cert.Subject.Organization[0] != "system:masters" {
		t.Fatalf("unexpected client organization: %v", cert.Subject.Organization)
	}
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Fatal("generated certificate is not for client authentication")
	}
	caCert := parseCertificate(t, caCertPEM)
	if err := cert.CheckSignatureFrom(caCert); err != nil {
		t.Fatalf("certificate is not signed by CA: %v", err)
	}
	if _, err := x509.ParsePKCS8PrivateKey(pemDecode(t, keyPEM).Bytes); err != nil {
		t.Fatal(err)
	}
}
