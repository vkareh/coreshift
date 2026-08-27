package certgen

import (
	"crypto/x509"
	"encoding/pem"
	"testing"
)

func TestGenerateCA(t *testing.T) {
	certPEM, keyPEM, err := GenerateCA()
	if err != nil {
		t.Fatal(err)
	}

	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		t.Fatal("generated certificate is not PEM encoded")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !cert.IsCA {
		t.Fatal("generated certificate is not a CA")
	}
	if err := cert.CheckSignatureFrom(cert); err != nil {
		t.Fatalf("certificate is not self-signed: %v", err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		t.Fatal("generated private key is not PEM encoded")
	}
	if _, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes); err != nil {
		t.Fatalf("generated private key cannot be parsed: %v", err)
	}
}
