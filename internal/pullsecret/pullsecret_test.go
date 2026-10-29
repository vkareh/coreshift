package pullsecret

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

// pullSecret is a minimal credentials file, the content the Secret
// stores verbatim.
var pullSecret = []byte(`{"auths":{"quay.io":{"auth":"dXNlcjpwYXNz"}}}`)

// storedSecret reads the Secret the client holds, failing the test when
// it stores none.
func storedSecret(t *testing.T, client *fake.Clientset) *corev1.Secret {
	t.Helper()
	secret, err := client.CoreV1().Secrets(secretNamespace).Get(context.Background(), secretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the stored Secret: %v", err)
	}
	return secret
}

// TestSeedStoresGlobalPullSecret checks the Secret's place and shape:
// the namespace, the name, the docker-config-json type, and the pull
// secret verbatim under the type's standard key.
func TestSeedStoresGlobalPullSecret(t *testing.T) {
	client := fake.NewSimpleClientset()

	if err := seedWithRetry(context.Background(), client, pullSecret); err != nil {
		t.Fatal(err)
	}

	if _, err := client.CoreV1().Namespaces().Get(context.Background(), secretNamespace, metav1.GetOptions{}); err != nil {
		t.Fatalf("the %s namespace was not created: %v", secretNamespace, err)
	}
	secret := storedSecret(t, client)
	if secret.Type != corev1.SecretTypeDockerConfigJson {
		t.Fatalf("secret type = %q, want the docker-config-json type", secret.Type)
	}
	if data := secret.Data[corev1.DockerConfigJsonKey]; !bytes.Equal(data, pullSecret) {
		t.Fatalf("secret data = %q, want the pull secret verbatim", data)
	}
}

// TestSeedReplacesChangedPullSecret checks that a changed pull secret
// file reaches the cluster on the next run, instead of the first stored
// one staying forever.
func TestSeedReplacesChangedPullSecret(t *testing.T) {
	client := fake.NewSimpleClientset()
	changed := []byte(`{"auths":{"registry.example":{"auth":"other"}}}`)

	if err := seedWithRetry(context.Background(), client, pullSecret); err != nil {
		t.Fatal(err)
	}
	if err := seedWithRetry(context.Background(), client, changed); err != nil {
		t.Fatal(err)
	}
	if data := storedSecret(t, client).Data[corev1.DockerConfigJsonKey]; !bytes.Equal(data, changed) {
		t.Fatalf("secret data = %q, want the changed pull secret", data)
	}
}

// TestSeedRetriesUntilAPIAcceptsWrites checks that a write rejected the
// way the API rejects early ones while it warms up is retried.
func TestSeedRetriesUntilAPIAcceptsWrites(t *testing.T) {
	objectRetryInterval = time.Millisecond
	t.Cleanup(func() { objectRetryInterval = 3 * time.Second })

	client := fake.NewSimpleClientset()
	var calls int
	client.PrependReactor("create", "secrets", func(_ ktesting.Action) (bool, runtime.Object, error) {
		calls++
		if calls == 1 {
			return true, nil, apierrors.NewForbidden(
				corev1.Resource("secrets"), secretName, errors.New("warming up"))
		}
		return false, nil, nil
	})

	if err := seedWithRetry(context.Background(), client, pullSecret); err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Fatalf("rejected write was not retried, calls: %d", calls)
	}
}

// TestSeedFailsOnPersistentError checks that a write the API never
// accepts fails the seed instead of retrying forever.
func TestSeedFailsOnPersistentError(t *testing.T) {
	objectRetryInterval = time.Millisecond
	t.Cleanup(func() { objectRetryInterval = 3 * time.Second })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "secrets", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("always refused")
	})

	if err := seedWithRetry(ctx, client, pullSecret); err == nil {
		t.Fatal("expected persistent error to fail the seed")
	}
}

// TestFetchReturnsStoredPullSecret checks that what Seed stores is what
// Fetch hands back, byte for byte.
func TestFetchReturnsStoredPullSecret(t *testing.T) {
	client := fake.NewSimpleClientset()
	if err := seedWithRetry(context.Background(), client, pullSecret); err != nil {
		t.Fatal(err)
	}

	data, err := fetch(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, pullSecret) {
		t.Fatalf("fetched %q, want the stored pull secret", data)
	}
}

// TestFetchWithoutStoredPullSecret checks that a cluster storing no
// Secret, the shape one created before the Secret existed has, answers
// nil rather than an error, leaving the caller to name the fallback.
func TestFetchWithoutStoredPullSecret(t *testing.T) {
	data, err := fetch(context.Background(), fake.NewSimpleClientset())
	if err != nil {
		t.Fatal(err)
	}
	if data != nil {
		t.Fatalf("fetched %q, want none", data)
	}
}

// TestFetchRefusesSecretWithoutKey checks that a Secret missing the
// standard key is refused as broken rather than read as empty.
func TestFetchRefusesSecretWithoutKey(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("get", "secrets", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: secretNamespace},
			Type:       corev1.SecretTypeDockerConfigJson,
		}, nil
	})

	_, err := fetch(context.Background(), client)
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte(corev1.DockerConfigJsonKey)) {
		t.Fatalf("err = %v, want a refusal naming the missing key", err)
	}
}
