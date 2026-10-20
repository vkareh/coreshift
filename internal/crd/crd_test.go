package crd

import (
	"context"
	"errors"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"
)

func TestSeedRegistersDefinitions(t *testing.T) {
	client := fake.NewSimpleClientset()

	if err := seed(context.Background(), client); err != nil {
		t.Fatal(err)
	}

	for _, want := range []struct {
		name, group, plural string
		scope               apiextensionsv1.ResourceScope
	}{
		{"clusterresourcequotas.quota.openshift.io", "quota.openshift.io", "clusterresourcequotas", apiextensionsv1.ClusterScoped},
		{"securitycontextconstraints.security.openshift.io", "security.openshift.io", "securitycontextconstraints", apiextensionsv1.ClusterScoped},
		{"rangeallocations.security.internal.openshift.io", "security.internal.openshift.io", "rangeallocations", apiextensionsv1.ClusterScoped},
		{"rolebindingrestrictions.authorization.openshift.io", "authorization.openshift.io", "rolebindingrestrictions", apiextensionsv1.NamespaceScoped},
		{"oauths.config.openshift.io", "config.openshift.io", "oauths", apiextensionsv1.ClusterScoped},
	} {
		definition, err := client.ApiextensionsV1().CustomResourceDefinitions().Get(context.Background(), want.name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("%s: %v", want.name, err)
		}
		if definition.Spec.Group != want.group {
			t.Fatalf("%s: unexpected group: %s", want.name, definition.Spec.Group)
		}
		if definition.Spec.Names.Plural != want.plural {
			t.Fatalf("%s: unexpected plural: %s", want.name, definition.Spec.Names.Plural)
		}
		if definition.Spec.Scope != want.scope {
			t.Fatalf("%s: unexpected scope: %s", want.name, definition.Spec.Scope)
		}
		if len(definition.Spec.Versions) != 1 || definition.Spec.Versions[0].Name != "v1" {
			t.Fatalf("%s: unexpected versions: %+v", want.name, definition.Spec.Versions)
		}
		if !definition.Spec.Versions[0].Served || !definition.Spec.Versions[0].Storage {
			t.Fatalf("%s: version v1 must be served and stored: %+v", want.name, definition.Spec.Versions[0])
		}
	}
}

func TestSeedIsRepeatable(t *testing.T) {
	client := fake.NewSimpleClientset()

	for range 2 {
		if err := seed(context.Background(), client); err != nil {
			t.Fatal(err)
		}
	}

	list, err := client.ApiextensionsV1().CustomResourceDefinitions().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != len(definitions) {
		t.Fatalf("expected the same definitions after reseeding, got %d", len(list.Items))
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
	for _, def := range definitions {
		if _, err := client.ApiextensionsV1().CustomResourceDefinitions().Get(context.Background(), def.name, metav1.GetOptions{}); err != nil {
			t.Fatalf("definition was not registered after retry: %v", err)
		}
	}
}

func TestWaitForEstablishedWaitsForTheAPI(t *testing.T) {
	objectRetryInterval = 10 * time.Millisecond
	t.Cleanup(func() { objectRetryInterval = 3 * time.Second })

	client := fake.NewSimpleClientset()

	// Answer the first check of each definition the way the API server
	// does before its definition controller has run, and mark it
	// established afterwards.
	gets := map[string]int{}
	client.PrependReactor("get", "customresourcedefinitions", func(action ktesting.Action) (bool, runtime.Object, error) {
		name := action.(ktesting.GetAction).GetName()
		gets[name]++
		definition := &apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: name},
		}
		if gets[name] > 1 {
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
	for _, def := range definitions {
		if gets[def.name] < 2 {
			t.Fatalf("establishment of %s was not polled, gets: %d", def.name, gets[def.name])
		}
	}
}
