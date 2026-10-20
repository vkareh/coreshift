package oauthclient

import (
	"context"
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func newClient() *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{clients: "OAuthClientList"},
	)
}

const testAPIURL = "https://127.0.0.1:7443"

func TestSeedCreatesLoginClient(t *testing.T) {
	client := newClient()

	if err := seedWithRetry(context.Background(), client, testAPIURL); err != nil {
		t.Fatal(err)
	}

	created, err := client.Resource(clients).Get(context.Background(), "openshift-challenging-client", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	grantMethod, _, err := unstructured.NestedString(created.Object, "grantMethod")
	if err != nil || grantMethod != "auto" {
		t.Fatalf("client grant method is %q (err %v), want auto", grantMethod, err)
	}
	redirects, _, err := unstructured.NestedStringSlice(created.Object, "redirectURIs")
	if err != nil || len(redirects) != 1 || redirects[0] != testAPIURL+"/oauth/token/implicit" {
		t.Fatalf("client redirect URIs are %v (err %v), want [%s]", redirects, err, testAPIURL+"/oauth/token/implicit")
	}
}

func TestSeedIsIdempotent(t *testing.T) {
	client := newClient()

	if err := seedWithRetry(context.Background(), client, testAPIURL); err != nil {
		t.Fatal(err)
	}
	if err := seedWithRetry(context.Background(), client, testAPIURL); err != nil {
		t.Fatal(err)
	}
}

func TestSeedRetriesUntilAPIAcceptsWrites(t *testing.T) {
	objectRetryInterval = time.Millisecond
	t.Cleanup(func() { objectRetryInterval = 3 * time.Second })

	client := newClient()
	// The first two writes are rejected the way a warming-up API server
	// rejects them; the seeding must retry and still succeed.
	rejections := 0
	client.PrependReactor("create", "oauthclients", func(action ktesting.Action) (bool, runtime.Object, error) {
		if rejections < 2 {
			rejections++
			return true, nil, apierrors.NewServiceUnavailable("warming up")
		}
		return false, nil, nil
	})

	if err := seedWithRetry(context.Background(), client, testAPIURL); err != nil {
		t.Fatal(err)
	}
	if rejections != 2 {
		t.Fatalf("expected two rejected writes, got %d", rejections)
	}
}

func TestSeedFailsOnPersistentError(t *testing.T) {
	objectRetryInterval = time.Millisecond
	t.Cleanup(func() { objectRetryInterval = 3 * time.Second })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	client := newClient()
	client.PrependReactor("create", "oauthclients", func(action ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("broken")
	})

	err := seedWithRetry(ctx, client, testAPIURL)
	if err == nil || errors.Unwrap(err).Error() != "broken" {
		t.Fatalf("expected the persistent error, got: %v", err)
	}
}
