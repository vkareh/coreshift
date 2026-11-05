package certgen

import (
	"crypto/x509"
	"testing"
)

func TestGenerateDNSMetricsCert(t *testing.T) {
	caCertPEM, caKeyPEM, err := GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := GenerateDNSMetricsCert(caCertPEM, caKeyPEM)
	if err != nil {
		t.Fatal(err)
	}

	cert := parseCertificate(t, certPEM)
	for _, name := range dnsMetricsDNSNames {
		if err := cert.VerifyHostname(name); err != nil {
			t.Fatalf("%s SAN missing: %v", name, err)
		}
	}
	var serverAuth bool
	for _, usage := range cert.ExtKeyUsage {
		serverAuth = serverAuth || usage == x509.ExtKeyUsageServerAuth
	}
	if !serverAuth {
		t.Fatal("generated certificate is not for server authentication")
	}
	if cert.Subject.CommonName != dnsMetricsDNSNames[0] {
		t.Fatalf("common name = %q, want the DNS service's name", cert.Subject.CommonName)
	}
	caCert := parseCertificate(t, caCertPEM)
	if err := cert.CheckSignatureFrom(caCert); err != nil {
		t.Fatalf("certificate is not signed by CA: %v", err)
	}
	if _, err := x509.ParsePKCS8PrivateKey(pemDecode(t, keyPEM).Bytes); err != nil {
		t.Fatal(err)
	}
}
