package dnstls

import (
	"bytes"
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

// dnsNamespace returns the operand namespace the DNS operator creates.
func dnsNamespace() *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: secretNamespace}}
}

// storedSecret reads the Secret the client holds, failing the test when
// it stores none.
func storedSecret(t *testing.T, client *fake.Clientset) *corev1.Secret {
	t.Helper()
	secret, err := client.CoreV1().Secrets(secretNamespace).Get(context.Background(), secretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the DNS metrics Secret: %v", err)
	}
	return secret
}

// TestSeedCreatesTheSecret checks the Secret's place and shape: the
// operand namespace, the name the DaemonSet's metrics volume names, the
// TLS type, and the certificate pair under the type's standard keys.
func TestSeedCreatesTheSecret(t *testing.T) {
	client := fake.NewSimpleClientset(dnsNamespace())
	certPEM, keyPEM := []byte("a certificate"), []byte("a key")

	if err := seedWithRetry(context.Background(), client, certPEM, keyPEM); err != nil {
		t.Fatal(err)
	}

	secret := storedSecret(t, client)
	if secret.Type != corev1.SecretTypeTLS {
		t.Fatalf("secret type = %q, want the TLS type", secret.Type)
	}
	if !bytes.Equal(secret.Data[corev1.TLSCertKey], certPEM) {
		t.Fatalf("certificate = %q, want the generated pair's certificate", secret.Data[corev1.TLSCertKey])
	}
	if !bytes.Equal(secret.Data[corev1.TLSPrivateKeyKey], keyPEM) {
		t.Fatalf("key = %q, want the generated pair's key", secret.Data[corev1.TLSPrivateKeyKey])
	}
}

// TestSeedWaitsForTheNamespace checks the seed waits for the DNS
// operator to create the operand namespace instead of failing on its
// absence.
func TestSeedWaitsForTheNamespace(t *testing.T) {
	objectRetryInterval = time.Millisecond
	t.Cleanup(func() { objectRetryInterval = 3 * time.Second })

	client := fake.NewSimpleClientset()
	created := make(chan struct{})
	go func() {
		if err := seedWithRetry(context.Background(), client, []byte("a certificate"), []byte("a key")); err != nil {
			t.Errorf("seed: %v", err)
		}
		close(created)
	}()
	select {
	case <-created:
		t.Fatal("seed finished while the namespace was still missing")
	case <-time.After(20 * time.Millisecond):
	}
	if err := client.Tracker().Add(dnsNamespace()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-created:
	case <-time.After(10 * time.Second):
		t.Fatal("seed did not finish after the namespace arrived")
	}
	storedSecret(t, client)
}

// TestSeedKeepsExistingSecret checks a warm boot: the Secret the first
// boot stored is never overwritten, the signer's own contract.
func TestSeedKeepsExistingSecret(t *testing.T) {
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: secretNamespace},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{corev1.TLSCertKey: []byte("the stored certificate")},
	}
	client := fake.NewSimpleClientset(dnsNamespace(), existing)

	if err := seedWithRetry(context.Background(), client, []byte("a fresh certificate"), []byte("a key")); err != nil {
		t.Fatal(err)
	}

	if cert := storedSecret(t, client).Data[corev1.TLSCertKey]; !bytes.Equal(cert, []byte("the stored certificate")) {
		t.Fatalf("certificate = %q, want the stored one kept", cert)
	}
}

// TestSeedFailsOnPersistentError checks that a write the API never
// accepts fails the seed instead of retrying forever.
func TestSeedFailsOnPersistentError(t *testing.T) {
	objectRetryInterval = time.Millisecond
	t.Cleanup(func() { objectRetryInterval = 3 * time.Second })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	client := fake.NewSimpleClientset(dnsNamespace())
	client.PrependReactor("create", "secrets", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(corev1.Resource("secrets"), secretName, nil)
	})

	if err := seedWithRetry(ctx, client, []byte("a certificate"), []byte("a key")); err == nil {
		t.Fatal("expected persistent error to fail the seed")
	}
}
