package certgen

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"time"
)

// serviceServingDNSNames lists the names the OpenShift API server and the
// OAuth API server are reached by, following the service DNS names OpenShift
// issues these serving certificates for.
var serviceServingDNSNames = []string{
	"openshift-apiserver",
	"openshift-apiserver.default.svc",
	"openshift-apiserver.default.svc.cluster.local",
	"openshift-oauth-apiserver",
	"openshift-oauth-apiserver.default.svc",
	"openshift-oauth-apiserver.default.svc.cluster.local",
}

// GenerateServiceServingCert creates the serving certificate shared by the
// OpenShift API server and the OAuth API server, signed by the CA certificate
// and key, both PEM encoded. OpenShift issues these certificates for both
// server and client authentication.
func GenerateServiceServingCert(caCertPEM, caKeyPEM []byte) (certPEM, keyPEM []byte, err error) {
	caCert, caKey, err := parseCA(caCertPEM, caKeyPEM)
	if err != nil {
		return nil, nil, err
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "openshift-apiserver",
			Organization: []string{"openshift"},
		},
		NotBefore: now,
		NotAfter:  now.Add(1 * 365 * 24 * time.Hour),
		DNSNames:  serviceServingDNSNames,
		KeyUsage:  x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		// Both usages: kube-apiserver dials these servers as a TLS client,
		// and OpenShift issues them as client-and-serving certificates.
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}
