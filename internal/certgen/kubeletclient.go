package certgen

import (
	"crypto/x509/pkix"
)

// GenerateKubeletClientCert creates the client certificate the
// kube-apiserver presents to kubelets, signed by the CA certificate and
// key, both PEM encoded. It carries the identity real clusters give the
// API server's kubelet connections, in the masters group so the kubelet's
// authorization webhook grants it node access.
func GenerateKubeletClientCert(caCertPEM, caKeyPEM []byte) (certPEM, keyPEM []byte, err error) {
	return generateClientCert(caCertPEM, caKeyPEM, pkix.Name{
		CommonName:   "system:kube-apiserver-kubelet-client",
		Organization: []string{"system:masters"},
	})
}
