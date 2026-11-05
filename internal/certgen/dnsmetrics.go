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

// dnsMetricsDNSNames lists the names the DNS DaemonSet's metrics
// sidecar serves its endpoint by, following the service names the
// serving certificate signer issues certificates for.
var dnsMetricsDNSNames = []string{
	"dns-default.openshift-dns.svc",
	"dns-default.openshift-dns.svc.cluster.local",
}

// GenerateDNSMetricsCert creates the serving certificate for the DNS
// DaemonSet's metrics sidecar, signed by the CA certificate and key,
// both PEM encoded. The signer that would issue it from the DNS
// Service's annotation does not run in this control plane, so the DNS
// seed stands in for its output with this certificate.
func GenerateDNSMetricsCert(caCertPEM, caKeyPEM []byte) (certPEM, keyPEM []byte, err error) {
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
		Subject:      pkix.Name{CommonName: dnsMetricsDNSNames[0]},
		NotBefore:    now,
		NotAfter:     now.Add(1 * 365 * 24 * time.Hour),
		DNSNames:     dnsMetricsDNSNames,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
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
