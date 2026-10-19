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
const (
	allocationController     = "system:openshift:controller:namespace-security-allocation-controller"
	allocationServiceAccount = "namespace-security-allocation-controller"
	infraNamespace           = "openshift-infra"
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
	return ensureClusterRoleBinding(ctx, client)
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
