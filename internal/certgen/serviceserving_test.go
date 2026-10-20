package certgen

import (
	"crypto/x509"
	"testing"
)

func TestGenerateServiceServingCert(t *testing.T) {
	caCertPEM, caKeyPEM, err := GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := GenerateServiceServingCert(caCertPEM, caKeyPEM)
	if err != nil {
		t.Fatal(err)
	}

	cert := parseCertificate(t, certPEM)
	for _, name := range serviceServingDNSNames {
		if err := cert.VerifyHostname(name); err != nil {
			t.Fatalf("%s SAN missing: %v", name, err)
		}
	}
	if err := cert.VerifyHostname("127.0.0.1"); err != nil {
		t.Fatalf("loopback SAN missing: %v", err)
	}
	var serverAuth, clientAuth bool
	for _, usage := range cert.ExtKeyUsage {
		serverAuth = serverAuth || usage == x509.ExtKeyUsageServerAuth
		clientAuth = clientAuth || usage == x509.ExtKeyUsageClientAuth
	}
	if !serverAuth || !clientAuth {
		t.Fatal("generated certificate is not for both server and client authentication")
	}
	caCert := parseCertificate(t, caCertPEM)
	if err := cert.CheckSignatureFrom(caCert); err != nil {
		t.Fatalf("certificate is not signed by CA: %v", err)
	}
	if _, err := x509.ParsePKCS8PrivateKey(pemDecode(t, keyPEM).Bytes); err != nil {
		t.Fatal(err)
	}
}
