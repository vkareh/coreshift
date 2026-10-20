package scc

import (
	"context"
	"errors"
	"slices"
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
		map[schema.GroupVersionResource]string{constraints: "SecurityContextConstraintsList"},
	)
}

func TestSeedRegistersConstraints(t *testing.T) {
	client := newClient()

	if err := seed(context.Background(), client); err != nil {
		t.Fatal(err)
	}

	list, err := client.Resource(constraints).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"anyuid", "hostaccess", "hostmount-anyuid", "hostmount-anyuid-v2",
		"hostnetwork", "hostnetwork-v2", "nested-container", "nonroot",
		"nonroot-v2", "privileged", "restricted", "restricted-v2", "restricted-v3",
	}
	got := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		if item.GetAPIVersion() != "security.openshift.io/v1" || item.GetKind() != "SecurityContextConstraints" {
			t.Fatalf("%s: unexpected type: %s %s", item.GetName(), item.GetAPIVersion(), item.GetKind())
		}
		got = append(got, item.GetName())
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("unexpected constraints: %v", got)
	}
}

func TestSeedIsRepeatable(t *testing.T) {
	client := newClient()

	for range 2 {
		if err := seed(context.Background(), client); err != nil {
			t.Fatal(err)
		}
	}

	list, err := client.Resource(constraints).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 13 {
		t.Fatalf("expected the same constraints after reseeding, got %d", len(list.Items))
	}
}

func TestSeedRetriesRejectedWrites(t *testing.T) {
	objectRetryInterval = 10 * time.Millisecond
	t.Cleanup(func() { objectRetryInterval = 3 * time.Second })

	client := newClient()

	// Reject the very first constraint write the way a warming
	// aggregated API does.
	var calls int
	client.PrependReactor("create", "securitycontextconstraints", func(action ktesting.Action) (bool, runtime.Object, error) {
		calls++
		if calls == 1 {
			return true, nil, apierrors.NewInternalError(errors.New("the server is currently unable to handle the request"))
		}
		return false, nil, nil
	})

	if err := seedWithRetry(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Fatalf("rejected write was not retried, calls: %d", calls)
	}
	list, err := client.Resource(constraints).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 13 {
		t.Fatalf("constraints were not registered after retry, got %d", len(list.Items))
	}
}
