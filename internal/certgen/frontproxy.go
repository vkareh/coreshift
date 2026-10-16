package certgen

import (
	"crypto/x509/pkix"
)

// GenerateFrontProxyCA creates the self-signed certificate authority that
// aggregated API servers use to recognize the kube-apiserver as a front
// proxy, plus its private key, both PEM encoded.
func GenerateFrontProxyCA() (certPEM, keyPEM []byte, err error) {
	return generateSelfSignedCA("front-proxy-ca")
}

// GenerateFrontProxyClientCert creates the client certificate the
// kube-apiserver presents to aggregated API servers when proxying requests
// to them, signed by the front-proxy CA certificate and key, both PEM
// encoded. OpenShift's aggregated API servers accept the
// system:openshift-aggregator identity, so it is used here.
func GenerateFrontProxyClientCert(caCertPEM, caKeyPEM []byte) (certPEM, keyPEM []byte, err error) {
	return generateClientCert(caCertPEM, caKeyPEM, pkix.Name{
		CommonName:   "system:openshift-aggregator",
		Organization: []string{"kubernetes"},
	})
}
