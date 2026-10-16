package certgen

import (
	"crypto/x509"
	"testing"
)

func TestGenerateFrontProxyCA(t *testing.T) {
	certPEM, keyPEM, err := GenerateFrontProxyCA()
	if err != nil {
		t.Fatal(err)
	}

	cert := parseCertificate(t, certPEM)
	if !cert.IsCA {
		t.Fatal("generated certificate is not a CA")
	}
	if cert.Subject.CommonName != "front-proxy-ca" {
		t.Fatalf("unexpected CA identity: %v", cert.Subject)
	}
	if err := cert.CheckSignatureFrom(cert); err != nil {
		t.Fatalf("certificate is not self-signed: %v", err)
	}
	if _, err := x509.ParsePKCS8PrivateKey(pemDecode(t, keyPEM).Bytes); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateFrontProxyClientCert(t *testing.T) {
	caCertPEM, caKeyPEM, err := GenerateFrontProxyCA()
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := GenerateFrontProxyClientCert(caCertPEM, caKeyPEM)
	if err != nil {
		t.Fatal(err)
	}

	cert := parseCertificate(t, certPEM)
	if cert.Subject.CommonName != "system:openshift-aggregator" {
		t.Fatalf("unexpected client identity: %v", cert.Subject)
	}
	if len(cert.Subject.Organization) != 1 || cert.Subject.Organization[0] != "kubernetes" {
		t.Fatalf("unexpected client organization: %v", cert.Subject.Organization)
	}
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Fatal("generated certificate is not for client authentication")
	}
	caCert := parseCertificate(t, caCertPEM)
	if err := cert.CheckSignatureFrom(caCert); err != nil {
		t.Fatalf("certificate is not signed by front-proxy CA: %v", err)
	}
	if _, err := x509.ParsePKCS8PrivateKey(pemDecode(t, keyPEM).Bytes); err != nil {
		t.Fatal(err)
	}
}
