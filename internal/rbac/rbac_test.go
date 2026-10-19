package rbac

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestSeedRegistersRoleAndBinding(t *testing.T) {
	client := fake.NewSimpleClientset()

	if err := seed(context.Background(), client); err != nil {
		t.Fatal(err)
	}

	role, err := client.RbacV1().ClusterRoles().Get(context.Background(), allocationController, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("cluster role: %v", err)
	}
	wantRules := []rbacv1.PolicyRule{
		{APIGroups: []string{"security.openshift.io", "security.internal.openshift.io"}, Resources: []string{"rangeallocations"}, Verbs: []string{"create", "get", "update"}},
		{APIGroups: []string{""}, Resources: []string{"namespaces"}, Verbs: []string{"get", "list", "watch", "update", "patch"}},
		{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"create", "patch", "update"}},
	}
	if !reflect.DeepEqual(role.Rules, wantRules) {
		t.Fatalf("cluster role has unexpected rules: %+v", role.Rules)
	}

	binding, err := client.RbacV1().ClusterRoleBindings().Get(context.Background(), allocationController, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("cluster role binding: %v", err)
	}
	wantSubjects := []rbacv1.Subject{{
		Kind:      rbacv1.ServiceAccountKind,
		Name:      "namespace-security-allocation-controller",
		Namespace: "openshift-infra",
	}}
	if !reflect.DeepEqual(binding.Subjects, wantSubjects) {
		t.Fatalf("binding has unexpected subjects: %+v", binding.Subjects)
	}
	if binding.RoleRef.Kind != "ClusterRole" || binding.RoleRef.Name != allocationController {
		t.Fatalf("binding points at unexpected role: %+v", binding.RoleRef)
	}
}

func TestSeedIsRepeatable(t *testing.T) {
	client := fake.NewSimpleClientset()

	for range 2 {
		if err := seed(context.Background(), client); err != nil {
			t.Fatal(err)
		}
	}

	roles, err := client.RbacV1().ClusterRoles().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(roles.Items) != 1 {
		t.Fatalf("expected the same one cluster role after reseeding, got %d", len(roles.Items))
	}
	bindings, err := client.RbacV1().ClusterRoleBindings().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings.Items) != 1 {
		t.Fatalf("expected the same one cluster role binding after reseeding, got %d", len(bindings.Items))
	}
}

func TestSeedRetriesRejectedWrites(t *testing.T) {
	objectRetryInterval = 10 * time.Millisecond
	t.Cleanup(func() { objectRetryInterval = 3 * time.Second })

	client := fake.NewSimpleClientset()

	// Reject the very first cluster role write the way a warming
	// apiserver does.
	var calls int
	client.PrependReactor("create", "clusterroles", func(action ktesting.Action) (bool, runtime.Object, error) {
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
	if _, err := client.RbacV1().ClusterRoles().Get(context.Background(), allocationController, metav1.GetOptions{}); err != nil {
		t.Fatalf("cluster role was not registered after retry: %v", err)
	}
}
