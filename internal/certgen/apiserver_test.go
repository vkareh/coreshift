package certgen

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"testing"
)

func TestGenerateAPIServerCert(t *testing.T) {
	caCertPEM, caKeyPEM, err := GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := GenerateAPIServerCert(caCertPEM, caKeyPEM)
	if err != nil {
		t.Fatal(err)
	}

	cert, err := x509.ParseCertificate(pemDecode(t, certPEM).Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := cert.VerifyHostname("localhost"); err != nil {
		t.Fatalf("localhost SAN missing: %v", err)
	}
	if err := cert.VerifyHostname("127.0.0.1"); err != nil {
		t.Fatalf("loopback SAN missing: %v", err)
	}
	if err := cert.VerifyHostname("kubernetes.default.svc"); err != nil {
		t.Fatalf("service DNS SAN missing: %v", err)
	}
	if len(cert.IPAddresses) != 1 || !cert.IPAddresses[0].Equal(net.ParseIP("127.0.0.1")) {
		t.Fatal("generated certificate has unexpected IP SANs")
	}
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Fatal("generated certificate is not for server authentication")
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
		t.Fatalf("generated private key cannot be parsed: %v", err)
	}
}
