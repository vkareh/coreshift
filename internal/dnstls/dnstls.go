// Package dnstls seeds the serving certificate Secret the CoreDNS
// DaemonSet's metrics sidecar mounts. The signer that would create it
// from the DNS Service's annotation — the service certificate signer,
// a payload Deployment this control plane does not run — is absent, so
// the seed stands in for its output the way the probed-MTU record
// stands in for the network operator's prober: a cluster input, not a
// payload manifest. Without the Secret no CoreDNS pod can start. The
// DNS operator creates the Secret's namespace on its first reconcile,
// so the seed waits for that and never overwrites what it finds.
package dnstls

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"

	"github.com/vkareh/coreshift/internal/certgen"
	"github.com/vkareh/coreshift/internal/warnings"
)

// The Secret's place and shape, matching what the DNS operator renders
// for the metrics sidecar to mount and what the signer would leave
// behind: a TLS Secret named for the DNS it belongs to, in the operand
// namespace the operator creates.
const (
	secretNamespace = "openshift-dns"
	secretName      = "dns-default-metrics-tls"
)

// objectRetryInterval spaces out retries while the DNS operator is
// still creating the objects the seed lands against. A variable for
// test override.
var objectRetryInterval = 3 * time.Second

// Seed stores the DNS metrics Secret through the admin kubeconfig: a
// serving certificate signed by the cluster's CA, standing in for the
// signer that does not run here. The write is retried until the DNS
// operator has created the namespace, and an existing Secret — a warm
// re-boot's — is left as it is.
func Seed(ctx context.Context, kubeconfigPath, stateDir string) error {
	config, err := warnings.Config(kubeconfigPath)
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create kube client: %w", err)
	}
	caCertPEM, err := os.ReadFile(filepath.Join(stateDir, certgen.CACertFilename))
	if err != nil {
		return fmt.Errorf("read the CA certificate: %w", err)
	}
	caKeyPEM, err := os.ReadFile(filepath.Join(stateDir, certgen.CAKeyFilename))
	if err != nil {
		return fmt.Errorf("read the CA key: %w", err)
	}
	certPEM, keyPEM, err := certgen.GenerateDNSMetricsCert(caCertPEM, caKeyPEM)
	if err != nil {
		return fmt.Errorf("generate the DNS metrics certificate: %w", err)
	}
	return seedWithRetry(ctx, client, certPEM, keyPEM)
}

// seedWithRetry keeps retrying until the context expires. The DNS
// operator decides when the write can land, so early failures are
// expected and retried.
func seedWithRetry(ctx context.Context, client kubernetes.Interface, certPEM, keyPEM []byte) error {
	var lastErr error
	pollErr := wait.PollUntilContextCancel(ctx, objectRetryInterval, true, func(ctx context.Context) (bool, error) {
		lastErr = seed(ctx, client, certPEM, keyPEM)
		return lastErr == nil, nil
	})
	if pollErr != nil {
		if lastErr == nil {
			return fmt.Errorf("seed the DNS metrics Secret: %w", ctx.Err())
		}
		return fmt.Errorf("seed the DNS metrics Secret: %w", lastErr)
	}
	return nil
}

func seed(ctx context.Context, client kubernetes.Interface, certPEM, keyPEM []byte) error {
	// The DNS operator creates the namespace on its first reconcile;
	// until then the Secret has nowhere to live.
	if _, err := client.CoreV1().Namespaces().Get(ctx, secretNamespace, metav1.GetOptions{}); err != nil {
		return err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: secretNamespace},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{corev1.TLSCertKey: certPEM, corev1.TLSPrivateKeyKey: keyPEM},
	}
	_, err := client.CoreV1().Secrets(secretNamespace).Create(ctx, secret, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}
