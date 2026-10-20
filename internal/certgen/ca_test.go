package certgen

import (
	"crypto/x509"
	"testing"
)

func TestGenerateCA(t *testing.T) {
	certPEM, keyPEM, err := GenerateCA()
	if err != nil {
		t.Fatal(err)
	}

	cert, err := x509.ParseCertificate(pemDecode(t, certPEM).Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !cert.IsCA {
		t.Fatal("generated certificate is not a CA")
	}
	if err := cert.CheckSignatureFrom(cert); err != nil {
		t.Fatalf("certificate is not self-signed: %v", err)
	}

	if _, err := x509.ParsePKCS8PrivateKey(pemDecode(t, keyPEM).Bytes); err != nil {
		t.Fatalf("generated private key cannot be parsed: %v", err)
	}
}
