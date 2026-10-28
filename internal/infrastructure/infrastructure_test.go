package infrastructure

import (
	"context"
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func newClient() *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{objects: "InfrastructureList"},
	)
}

func TestSeedCreatesClusterObject(t *testing.T) {
	client := newClient()

	if err := seedWithRetry(context.Background(), client); err != nil {
		t.Fatal(err)
	}

	created, err := client.Resource(objects).Get(context.Background(), "cluster", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if created.Object["spec"] == nil {
		t.Fatalf("object spec = %v, want the payload's empty platform-less spec", created.Object["spec"])
	}
}

func TestSeedIsIdempotent(t *testing.T) {
	client := newClient()

	for range 2 {
		if err := seedWithRetry(context.Background(), client); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSeedRetriesUntilAPIAcceptsWrites(t *testing.T) {
	objectRetryInterval = time.Millisecond
	t.Cleanup(func() { objectRetryInterval = 3 * time.Second })

	client := newClient()

	// Reject the very first write the way the API does before the
	// definition behind it is established.
	var calls int
	client.PrependReactor("create", "infrastructures", func(action ktesting.Action) (bool, runtime.Object, error) {
		calls++
		if calls == 1 {
			return true, nil, apierrors.NewNotFound(objects.GroupResource(), "cluster")
		}
		return false, nil, nil
	})

	if err := seedWithRetry(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Fatalf("rejected write was not retried, calls: %d", calls)
	}
}

func TestSeedFailsOnPersistentError(t *testing.T) {
	objectRetryInterval = time.Millisecond
	t.Cleanup(func() { objectRetryInterval = 3 * time.Second })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	client := newClient()
	client.PrependReactor("create", "infrastructures", func(action ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("always refused")
	})

	if err := seedWithRetry(ctx, client); err == nil {
		t.Fatal("expected persistent error to fail the seed")
	}
}
