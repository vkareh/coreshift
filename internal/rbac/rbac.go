// Package rbac seeds the cluster-wide permissions that OpenShift
// controllers expect to already exist when they start. In real
// deployments Hypershift's hosted-cluster-config-operator applies them;
// this package does the equivalent for a locally run control plane.
package rbac

import (
	"context"
	"fmt"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// objectRetryInterval spaces out retries while kube-apiserver warms up or
// rejects early writes. A variable for test override.
var objectRetryInterval = 3 * time.Second

// The cluster policy controller's namespace-security-allocation
// controller impersonates a service account that must hold these
// permissions; without them its calls are rejected until it gives up.
// The pod security label syncers below run in the same binary and are
// seeded the same way.
const (
	allocationController     = "system:openshift:controller:namespace-security-allocation-controller"
	allocationServiceAccount = "namespace-security-allocation-controller"
	infraNamespace           = "openshift-infra"

	labelSyncController     = "system:openshift:controller:podsecurity-admission-label-syncer-controller"
	labelSyncServiceAccount = "podsecurity-admission-label-syncer-controller"

	privilegedSyncer        = "system:openshift:controller:privileged-namespaces-psa-label-syncer"
	privilegedSyncerAccount = "privileged-namespaces-psa-label-syncer"

	restrictedConstraint = "system:openshift:scc:restricted-v3"
)

// Seed creates the RBAC objects the locally run controllers need. The
// writes are retried and idempotent, so it is safe to call while the API
// server is still coming up.
func Seed(ctx context.Context, kubeconfigPath string) error {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create kube client: %w", err)
	}
	return seedWithRetry(ctx, kubeClient)
}

// seedWithRetry keeps retrying until the context expires. The apiserver
// can reject the first writes while it warms up.
func seedWithRetry(ctx context.Context, kubeClient kubernetes.Interface) error {
	var lastErr error
	pollErr := wait.PollUntilContextCancel(ctx, objectRetryInterval, true, func(ctx context.Context) (bool, error) {
		lastErr = seed(ctx, kubeClient)
		return lastErr == nil, nil
	})
	if pollErr != nil {
		if lastErr == nil {
			return fmt.Errorf("seed controller RBAC: %w", ctx.Err())
		}
		return fmt.Errorf("seed controller RBAC: %w", lastErr)
	}
	return nil
}

func seed(ctx context.Context, client kubernetes.Interface) error {
	if err := ensureClusterRole(ctx, client); err != nil {
		return err
	}
	if err := ensureClusterRoleBinding(ctx, client); err != nil {
		return err
	}
	if err := ensureLabelSyncer(ctx, client); err != nil {
		return err
	}
	if err := ensurePrivilegedSyncer(ctx, client); err != nil {
		return err
	}
	return ensureRestrictedConstraint(ctx, client)
}

// ensureClusterRole grants what the namespace-security-allocation
// controller needs to hand out UID and GID ranges: writing the range
// allocations, labeling namespaces with the assigned range, and recording
// events. It is intentionally left untouched when it already exists.
func ensureClusterRole(ctx context.Context, client kubernetes.Interface) error {
	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: allocationController},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"security.openshift.io", "security.internal.openshift.io"},
				Resources: []string{"rangeallocations"},
				Verbs:     []string{"create", "get", "update"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"namespaces"},
				Verbs:     []string{"get", "list", "watch", "update", "patch"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"events"},
				Verbs:     []string{"create", "patch", "update"},
			},
		},
	}
	_, err := client.RbacV1().ClusterRoles().Create(ctx, role, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("register %s cluster role: %w", allocationController, err)
	}
	return nil
}

// ensureClusterRoleBinding grants the role to the impersonated service
// account. The namespace and the account itself are created by the
// controllers; the binding is what is missing here.
func ensureClusterRoleBinding(ctx context.Context, client kubernetes.Interface) error {
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: allocationController},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      allocationServiceAccount,
			Namespace: infraNamespace,
		}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     allocationController,
		},
	}
	_, err := client.RbacV1().ClusterRoleBindings().Create(ctx, binding, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("register %s cluster role binding: %w", allocationController, err)
	}
	return nil
}

// ensureLabelSyncer grants what the pod security admission label syncer
// needs to set the pod security labels on a namespace from the security
// constraints its service accounts may use: reading the namespaces,
// accounts, constraints, and RBAC involved, updating the labels, and
// recording events. Copied verbatim from the openshift
// cluster-kube-controller-manager-operator repository at the
// release-4.22 branch, which applies the same pair in real clusters.
func ensureLabelSyncer(ctx context.Context, client kubernetes.Interface) error {
	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: labelSyncController},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"namespaces"},
				Verbs:     []string{"get", "list", "update", "watch", "patch"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"events"},
				Verbs:     []string{"create", "patch", "update"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"serviceaccounts"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{"security.openshift.io"},
				Resources: []string{"securitycontextconstraints"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{"rbac.authorization.k8s.io"},
				Resources: []string{"clusterroles", "clusterrolebindings", "roles", "rolebindings"},
				Verbs:     []string{"get", "list", "watch"},
			},
		},
	}
	if _, err := client.RbacV1().ClusterRoles().Create(ctx, role, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("register %s cluster role: %w", labelSyncController, err)
	}
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: labelSyncController},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      labelSyncServiceAccount,
			Namespace: infraNamespace,
		}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     labelSyncController,
		},
	}
	_, err := client.RbacV1().ClusterRoleBindings().Create(ctx, binding, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("register %s cluster role binding: %w", labelSyncController, err)
	}
	return nil
}

// ensurePrivilegedSyncer grants what the privileged namespaces label
// syncer needs to stamp the fixed pod security labels onto the default,
// kube-system, and kube-public namespaces. Without it every attempt is
// rejected and retried forever. Copied verbatim from the same
// cluster-kube-controller-manager-operator manifests as the pair above.
func ensurePrivilegedSyncer(ctx context.Context, client kubernetes.Interface) error {
	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: privilegedSyncer},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"namespaces"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups:     []string{""},
				Resources:     []string{"namespaces"},
				ResourceNames: []string{"default", "kube-system", "kube-public"},
				Verbs:         []string{"patch"},
			},
		},
	}
	if _, err := client.RbacV1().ClusterRoles().Create(ctx, role, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("register %s cluster role: %w", privilegedSyncer, err)
	}
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: privilegedSyncer},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      privilegedSyncerAccount,
			Namespace: infraNamespace,
		}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     privilegedSyncer,
		},
	}
	_, err := client.RbacV1().ClusterRoleBindings().Create(ctx, binding, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("register %s cluster role binding: %w", privilegedSyncer, err)
	}
	return nil
}

// ensureRestrictedConstraint grants every signed-in user use of the
// default restricted security constraint, the same grant the release
// payload's kube-apiserver operator manifests carry. The label syncer
// builds each namespace's label level from these grants; without one it
// cannot classify a namespace at all and retries in error.
func ensureRestrictedConstraint(ctx context.Context, client kubernetes.Interface) error {
	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: restrictedConstraint},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups:     []string{"security.openshift.io"},
				Resources:     []string{"securitycontextconstraints"},
				ResourceNames: []string{"restricted-v3"},
				Verbs:         []string{"use"},
			},
		},
	}
	if _, err := client.RbacV1().ClusterRoles().Create(ctx, role, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("register %s cluster role: %w", restrictedConstraint, err)
	}
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: restrictedConstraint},
		Subjects: []rbacv1.Subject{{
			APIGroup: rbacv1.GroupName,
			Kind:     rbacv1.GroupKind,
			Name:     "system:authenticated",
		}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     restrictedConstraint,
		},
	}
	_, err := client.RbacV1().ClusterRoleBindings().Create(ctx, binding, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("register %s cluster role binding: %w", restrictedConstraint, err)
	}
	return nil
}
