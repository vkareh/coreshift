package certgen

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	clientcmd "k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// GenerateClientCert creates a cluster-admin client certificate signed by the
// CA certificate and key, both PEM encoded.
func GenerateClientCert(caCertPEM, caKeyPEM []byte) (certPEM, keyPEM []byte, err error) {
	return generateClientCert(caCertPEM, caKeyPEM, pkix.Name{
		CommonName:   "admin",
		Organization: []string{"system:masters"},
	})
}

// generateClientCert creates a client certificate with the given subject
// signed by the CA certificate and key, both PEM encoded.
func generateClientCert(caCertPEM, caKeyPEM []byte, subject pkix.Name) (certPEM, keyPEM []byte, err error) {
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
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      subject,
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(1 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
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

// GenerateAdminKubeconfig creates a directly usable kubeconfig for the
// cluster-admin client.
func GenerateAdminKubeconfig(caCertPEM, caKeyPEM []byte) ([]byte, error) {
	clientCertPEM, clientKeyPEM, err := GenerateClientCert(caCertPEM, caKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("generate client certificate: %w", err)
	}
	return generateKubeconfig(defaultAPIServerURL, "admin", defaultClusterName, caCertPEM, clientCertPEM, clientKeyPEM)
}

// generateComponentKubeconfig creates a directly usable kubeconfig for a
// control-plane component client identified by commonName. The embedded
// client certificate carries no organization, unlike the admin client: these
// identities get their permissions from kube-apiserver's built-in bootstrap
// RBAC, so a privileged group would be unnecessary over-privilege.
func generateComponentKubeconfig(commonName, serverURL, clusterName string, caCertPEM, caKeyPEM []byte) ([]byte, error) {
	clientCertPEM, clientKeyPEM, err := generateClientCert(caCertPEM, caKeyPEM, pkix.Name{CommonName: commonName})
	if err != nil {
		return nil, fmt.Errorf("generate client certificate: %w", err)
	}
	return generateKubeconfig(serverURL, commonName, clusterName, caCertPEM, clientCertPEM, clientKeyPEM)
}

// generateAdminKubeconfig creates a directly usable kubeconfig for a
// control-plane component that Hypershift runs with its admin kubeconfig:
// the aggregated API servers, the OAuth server, and the two controller
// managers. These components run as the system:admin identity so their
// calls work without seeding any component-specific RBAC.
func generateAdminKubeconfig(serverURL, clusterName string, caCertPEM, caKeyPEM []byte) ([]byte, error) {
	clientCertPEM, clientKeyPEM, err := generateClientCert(caCertPEM, caKeyPEM, pkix.Name{
		CommonName:   "system:admin",
		Organization: []string{"system:masters"},
	})
	if err != nil {
		return nil, fmt.Errorf("generate client certificate: %w", err)
	}
	return generateKubeconfig(serverURL, "system:admin", clusterName, caCertPEM, clientCertPEM, clientKeyPEM)
}

// generateKubeconfig creates a directly usable kubeconfig for a user
// against the named cluster. Every entry — cluster, context, and user —
// carries the cluster's name, so the kubeconfig files of several clusters
// merge without colliding.
func generateKubeconfig(serverURL, user, clusterName string, caCertPEM, clientCertPEM, clientKeyPEM []byte) ([]byte, error) {
	config := clientcmdapi.NewConfig()
	config.Clusters[clusterName] = &clientcmdapi.Cluster{
		Server:                   serverURL,
		CertificateAuthorityData: caCertPEM,
	}
	config.AuthInfos[user+"@"+clusterName] = &clientcmdapi.AuthInfo{
		ClientCertificateData: clientCertPEM,
		ClientKeyData:         clientKeyPEM,
	}
	config.Contexts[user+"@"+clusterName] = &clientcmdapi.Context{
		Cluster:  clusterName,
		AuthInfo: user + "@" + clusterName,
	}
	config.CurrentContext = user + "@" + clusterName

	return clientcmd.Write(*config)
}

func parseCA(certPEM, keyPEM []byte) (*x509.Certificate, *rsa.PrivateKey, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, nil, fmt.Errorf("CA certificate is not PEM encoded")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA certificate: %w", err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, fmt.Errorf("CA key is not PEM encoded")
	}
	key, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA key: %w", err)
	}
	privateKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, nil, fmt.Errorf("CA key is not an RSA private key")
	}
	return cert, privateKey, nil
}
