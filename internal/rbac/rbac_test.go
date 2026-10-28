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
	"k8s.io/client-go/kubernetes"
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

func TestSeedRegistersLabelSyncer(t *testing.T) {
	client := fake.NewSimpleClientset()

	if err := seed(context.Background(), client); err != nil {
		t.Fatal(err)
	}

	role, err := client.RbacV1().ClusterRoles().Get(context.Background(), labelSyncController, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("cluster role: %v", err)
	}
	wantRules := []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"namespaces"}, Verbs: []string{"get", "list", "update", "watch", "patch"}},
		{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"create", "patch", "update"}},
		{APIGroups: []string{""}, Resources: []string{"serviceaccounts"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{"security.openshift.io"}, Resources: []string{"securitycontextconstraints"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{"rbac.authorization.k8s.io"}, Resources: []string{"clusterroles", "clusterrolebindings", "roles", "rolebindings"}, Verbs: []string{"get", "list", "watch"}},
	}
	if !reflect.DeepEqual(role.Rules, wantRules) {
		t.Fatalf("cluster role has unexpected rules: %+v", role.Rules)
	}
	assertBoundToServiceAccount(t, client, labelSyncController, labelSyncServiceAccount)
}

func TestSeedRegistersPrivilegedSyncer(t *testing.T) {
	client := fake.NewSimpleClientset()

	if err := seed(context.Background(), client); err != nil {
		t.Fatal(err)
	}

	role, err := client.RbacV1().ClusterRoles().Get(context.Background(), privilegedSyncer, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("cluster role: %v", err)
	}
	wantRules := []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"namespaces"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{""}, Resources: []string{"namespaces"}, ResourceNames: []string{"default", "kube-system", "kube-public"}, Verbs: []string{"patch"}},
	}
	if !reflect.DeepEqual(role.Rules, wantRules) {
		t.Fatalf("cluster role has unexpected rules: %+v", role.Rules)
	}
	assertBoundToServiceAccount(t, client, privilegedSyncer, privilegedSyncerAccount)
}

func TestSeedRegistersRestrictedConstraint(t *testing.T) {
	client := fake.NewSimpleClientset()

	if err := seed(context.Background(), client); err != nil {
		t.Fatal(err)
	}

	role, err := client.RbacV1().ClusterRoles().Get(context.Background(), restrictedConstraint, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("cluster role: %v", err)
	}
	wantRules := []rbacv1.PolicyRule{
		{APIGroups: []string{"security.openshift.io"}, Resources: []string{"securitycontextconstraints"}, ResourceNames: []string{"restricted-v3"}, Verbs: []string{"use"}},
	}
	if !reflect.DeepEqual(role.Rules, wantRules) {
		t.Fatalf("cluster role has unexpected rules: %+v", role.Rules)
	}

	binding, err := client.RbacV1().ClusterRoleBindings().Get(context.Background(), restrictedConstraint, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("cluster role binding: %v", err)
	}
	wantSubjects := []rbacv1.Subject{{
		APIGroup: rbacv1.GroupName,
		Kind:     rbacv1.GroupKind,
		Name:     "system:authenticated",
	}}
	if !reflect.DeepEqual(binding.Subjects, wantSubjects) {
		t.Fatalf("binding has unexpected subjects: %+v", binding.Subjects)
	}
	if binding.RoleRef.Kind != "ClusterRole" || binding.RoleRef.Name != restrictedConstraint {
		t.Fatalf("binding points at unexpected role: %+v", binding.RoleRef)
	}
}

// assertBoundToServiceAccount checks that the named binding grants the
// matching role to the given service account in the infra namespace.
func assertBoundToServiceAccount(t *testing.T, client kubernetes.Interface, name, serviceAccount string) {
	t.Helper()
	binding, err := client.RbacV1().ClusterRoleBindings().Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("cluster role binding: %v", err)
	}
	wantSubjects := []rbacv1.Subject{{
		Kind:      rbacv1.ServiceAccountKind,
		Name:      serviceAccount,
		Namespace: infraNamespace,
	}}
	if !reflect.DeepEqual(binding.Subjects, wantSubjects) {
		t.Fatalf("binding has unexpected subjects: %+v", binding.Subjects)
	}
	if binding.RoleRef.Kind != "ClusterRole" || binding.RoleRef.Name != name {
		t.Fatalf("binding points at unexpected role: %+v", binding.RoleRef)
	}
}

func TestSeedRegistersMachineApprover(t *testing.T) {
	client := fake.NewSimpleClientset()

	if err := seed(context.Background(), client); err != nil {
		t.Fatal(err)
	}

	role, err := client.RbacV1().ClusterRoles().Get(context.Background(), machineApprover, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("cluster role: %v", err)
	}
	wantRules := []rbacv1.PolicyRule{
		{APIGroups: []string{"certificates.k8s.io"}, Resources: []string{"certificatesigningrequests"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{"certificates.k8s.io"}, Resources: []string{"certificatesigningrequests/approval"}, Verbs: []string{"update"}},
		{APIGroups: []string{"certificates.k8s.io"}, Resources: []string{"signers"}, ResourceNames: []string{"kubernetes.io/kube-apiserver-client-kubelet", "kubernetes.io/kubelet-serving"}, Verbs: []string{"approve"}},
		{APIGroups: []string{"machine.openshift.io"}, Resources: []string{"machines"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get", "watch", "list"}},
		{APIGroups: []string{"authentication.k8s.io"}, Resources: []string{"tokenreviews"}, Verbs: []string{"create"}},
		{APIGroups: []string{"authorization.k8s.io"}, Resources: []string{"subjectaccessreviews"}, Verbs: []string{"create"}},
		{APIGroups: []string{"config.openshift.io"}, Resources: []string{"clusteroperators"}, Verbs: []string{"get", "create", "list", "watch"}},
		{APIGroups: []string{"config.openshift.io"}, Resources: []string{"clusteroperators/status"}, ResourceNames: []string{"machine-approver"}, Verbs: []string{"update"}},
		{APIGroups: []string{"config.openshift.io"}, Resources: []string{"networks"}, Verbs: []string{"get"}},
		{APIGroups: []string{"network.openshift.io"}, Resources: []string{"hostsubnets"}, Verbs: []string{"get"}},
	}
	if !reflect.DeepEqual(role.Rules, wantRules) {
		t.Fatalf("cluster role has unexpected rules: %+v", role.Rules)
	}

	binding, err := client.RbacV1().ClusterRoleBindings().Get(context.Background(), machineApprover, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("cluster role binding: %v", err)
	}
	wantSubjects := []rbacv1.Subject{{
		Kind: rbacv1.UserKind,
		Name: machineApprover,
	}}
	if !reflect.DeepEqual(binding.Subjects, wantSubjects) {
		t.Fatalf("binding has unexpected subjects: %+v", binding.Subjects)
	}
	if binding.RoleRef.Kind != "ClusterRole" || binding.RoleRef.Name != machineApprover {
		t.Fatalf("binding points at unexpected role: %+v", binding.RoleRef)
	}
}

func TestSeedRegistersNodeBootstrapper(t *testing.T) {
	client := fake.NewSimpleClientset()

	if err := seed(context.Background(), client); err != nil {
		t.Fatal(err)
	}

	binding, err := client.RbacV1().ClusterRoleBindings().Get(context.Background(), nodeBootstrapperBinding, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("cluster role binding: %v", err)
	}
	wantSubjects := []rbacv1.Subject{{
		Kind:      rbacv1.ServiceAccountKind,
		Name:      nodeBootstrapperAccount,
		Namespace: mcoNamespace,
	}}
	if !reflect.DeepEqual(binding.Subjects, wantSubjects) {
		t.Fatalf("binding has unexpected subjects: %+v", binding.Subjects)
	}
	if binding.RoleRef.Kind != "ClusterRole" || binding.RoleRef.Name != "system:node-bootstrapper" {
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
	if len(roles.Items) != 5 {
		t.Fatalf("expected the same five cluster roles after reseeding, got %d", len(roles.Items))
	}
	bindings, err := client.RbacV1().ClusterRoleBindings().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings.Items) != 6 {
		t.Fatalf("expected the same six cluster role bindings after reseeding, got %d", len(bindings.Items))
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
