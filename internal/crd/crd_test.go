package crd

import (
	"context"
	"errors"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestSeedRegistersDefinition(t *testing.T) {
	client := fake.NewSimpleClientset()

	if err := seed(context.Background(), client); err != nil {
		t.Fatal(err)
	}

	definition, err := client.ApiextensionsV1().CustomResourceDefinitions().Get(context.Background(), rangeAllocationName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("definition: %v", err)
	}
	if definition.Spec.Group != "security.internal.openshift.io" {
		t.Fatalf("unexpected group: %s", definition.Spec.Group)
	}
	if definition.Spec.Names.Kind != "RangeAllocation" || definition.Spec.Names.Plural != "rangeallocations" {
		t.Fatalf("unexpected names: %+v", definition.Spec.Names)
	}
	if definition.Spec.Scope != apiextensionsv1.ClusterScoped {
		t.Fatalf("unexpected scope: %s", definition.Spec.Scope)
	}
	if len(definition.Spec.Versions) != 1 || definition.Spec.Versions[0].Name != "v1" {
		t.Fatalf("unexpected versions: %+v", definition.Spec.Versions)
	}
	if !definition.Spec.Versions[0].Served || !definition.Spec.Versions[0].Storage {
		t.Fatalf("version v1 must be served and stored: %+v", definition.Spec.Versions[0])
	}
}

func TestSeedIsRepeatable(t *testing.T) {
	client := fake.NewSimpleClientset()

	for range 2 {
		if err := seed(context.Background(), client); err != nil {
			t.Fatal(err)
		}
	}

	definitions, err := client.ApiextensionsV1().CustomResourceDefinitions().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(definitions.Items) != 1 {
		t.Fatalf("expected the same one definition after reseeding, got %d", len(definitions.Items))
	}
}

func TestSeedRetriesRejectedWrites(t *testing.T) {
	objectRetryInterval = 10 * time.Millisecond
	t.Cleanup(func() { objectRetryInterval = 3 * time.Second })

	client := fake.NewSimpleClientset()

	// Reject the very first definition write the way a warming apiserver
	// does.
	var calls int
	client.PrependReactor("create", "customresourcedefinitions", func(action ktesting.Action) (bool, runtime.Object, error) {
		calls++
		if calls == 1 {
			return true, nil, apierrors.NewInternalError(errors.New("etcdserver: request timed out"))
		}
		return false, nil, nil
	})

	if err := seedWithRetry(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Fatalf("rejected write was not retried, calls: %d", calls)
	}
	if _, err := client.ApiextensionsV1().CustomResourceDefinitions().Get(context.Background(), rangeAllocationName, metav1.GetOptions{}); err != nil {
		t.Fatalf("definition was not registered after retry: %v", err)
	}
}

func TestWaitForEstablishedWaitsForTheAPI(t *testing.T) {
	objectRetryInterval = 10 * time.Millisecond
	t.Cleanup(func() { objectRetryInterval = 3 * time.Second })

	client := fake.NewSimpleClientset()

	// Answer the first check the way the API server does before its
	// definition controller has run, and mark it established afterwards.
	var gets int
	client.PrependReactor("get", "customresourcedefinitions", func(action ktesting.Action) (bool, runtime.Object, error) {
		gets++
		definition := &apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: rangeAllocationName},
		}
		if gets > 1 {
			definition.Status.Conditions = []apiextensionsv1.CustomResourceDefinitionCondition{{
				Type:   apiextensionsv1.Established,
				Status: apiextensionsv1.ConditionTrue,
			}}
		}
		return true, definition, nil
	})

	if err := waitForEstablished(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	if gets < 2 {
		t.Fatalf("establishment was not polled, gets: %d", gets)
	}
}
