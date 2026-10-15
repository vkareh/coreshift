package certgen

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"testing"
)

func TestGenerateServiceAccountKeyPair(t *testing.T) {
	keyPEM, pubPEM, err := GenerateServiceAccountKeyPair()
	if err != nil {
		t.Fatal(err)
	}

	keyBlock := pemDecode(t, keyPEM)
	if keyBlock.Type != "PRIVATE KEY" {
		t.Fatalf("private key PEM block type is %q, want %q", keyBlock.Type, "PRIVATE KEY")
	}
	key, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatalf("generated private key cannot be parsed: %v", err)
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		t.Fatal("generated private key is not an RSA private key")
	}
	if rsaKey.N.BitLen() != 2048 {
		t.Fatalf("private key is %d bits, want 2048", rsaKey.N.BitLen())
	}

	pubBlock := pemDecode(t, pubPEM)
	if pubBlock.Type != "PUBLIC KEY" {
		t.Fatalf("public key PEM block type is %q, want %q", pubBlock.Type, "PUBLIC KEY")
	}
	pub, err := x509.ParsePKIXPublicKey(pubBlock.Bytes)
	if err != nil {
		t.Fatalf("generated public key cannot be parsed: %v", err)
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		t.Fatal("generated public key is not an RSA public key")
	}

	message := []byte("coreshift service-account key pair")
	digest := sha256.Sum256(message)
	signature, err := rsa.SignPKCS1v15(rand.Reader, rsaKey, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign with private key: %v", err)
	}
	if err := rsa.VerifyPKCS1v15(rsaPub, crypto.SHA256, digest[:], signature); err != nil {
		t.Fatalf("public key does not match private key: %v", err)
	}

	other := sha256.Sum256([]byte("not the signed message"))
	if err := rsa.VerifyPKCS1v15(rsaPub, crypto.SHA256, other[:], signature); err == nil {
		t.Fatal("signature verified against a different message")
	}
}
