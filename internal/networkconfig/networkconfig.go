// Package networkconfig seeds the network configuration the cluster
// network operator reads. The release payload carries the cluster
// network object only as a create-only empty shell — the ranges are
// installer output, and the operator refuses an empty configuration —
// and nothing carries the probed host MTU, which the operator's prober
// job records from a node this control plane does not have. This
// package stands in for both the way the pull secret and the OAuth
// client stand in for theirs: cluster inputs, not payload manifests.
package networkconfig

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

// The network configuration a default install carries: the service
// range the API server's own flag already runs with, and the pod range,
// host prefix, and network type the payload defaults to when an install
// config names none.
const (
	serviceNetwork    = "172.30.0.0/16"
	clusterNetwork    = "10.128.0.0/14"
	clusterHostPrefix = 23
	networkType       = "OVNKubernetes"
)

// probedMTU stands in for the host MTU the operator's prober job would
// record from a node: a standard Ethernet interface's, the value the
// payload's default network MTU assumes beneath its encapsulation.
const probedMTU = "1500"

// The objects the seed writes: the cluster network configuration and,
// in the operator's namespace, the record the prober leaves behind.
var (
	networks = schema.GroupVersionResource{
		Group:    "config.openshift.io",
		Version:  "v1",
		Resource: "networks",
	}
	configMaps = schema.GroupVersionResource{
		Version:  "v1",
		Resource: "configmaps",
	}
	probedMTUNamespace = "openshift-network-operator"
	probedMTUName      = "mtu"
)

// objectRetryInterval spaces out retries while the version operator is
// still delivering the definitions and namespaces these writes land
// against. A variable for test override.
var objectRetryInterval = 3 * time.Second

// Seed writes the network configuration through the admin kubeconfig:
// the cluster network object with the ranges the control plane runs
// with, then the probed MTU record. Both writes are retried and
// idempotent, and an object that already carries its configuration —
// a warm re-boot's — is left exactly as it is.
func Seed(ctx context.Context, kubeconfigPath string) error {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create dynamic client: %w", err)
	}
	return seedWithRetry(ctx, client)
}

// seedWithRetry keeps retrying until the context expires. The version
// operator's delivery decides when the writes can land, so early
// failures are expected and retried.
func seedWithRetry(ctx context.Context, client dynamic.Interface) error {
	var lastErr error
	pollErr := wait.PollUntilContextCancel(ctx, objectRetryInterval, true, func(ctx context.Context) (bool, error) {
		if lastErr = seed(ctx, client); lastErr != nil {
			return false, nil
		}
		return true, nil
	})
	if pollErr != nil {
		if lastErr == nil {
			return fmt.Errorf("seed network configuration: %w", ctx.Err())
		}
		return fmt.Errorf("seed network configuration: %w", lastErr)
	}
	return nil
}

func seed(ctx context.Context, client dynamic.Interface) error {
	if err := ensureNetwork(ctx, client); err != nil {
		return err
	}
	return ensureProbedMTU(ctx, client)
}

// networkSpec returns the spec the installer writes for a default
// OVN-Kubernetes install, as JSON-shaped values the API round-trips.
func networkSpec() map[string]any {
	return map[string]any{
		"networkType":    networkType,
		"serviceNetwork": []any{serviceNetwork},
		"clusterNetwork": []any{
			map[string]any{"cidr": clusterNetwork, "hostPrefix": int64(clusterHostPrefix)},
		},
	}
}

// ensureNetwork stores the cluster network configuration. The version
// operator delivers the object as a create-only empty shell, so whoever
// lands first fills it: a missing object is created carrying the spec,
// and an empty one — the payload's shell, before or after the operator
// adopted it — has the spec filled in. An object that already carries a
// network type, a warm re-boot's or one the operator already reconciled,
// is left untouched.
func ensureNetwork(ctx context.Context, client dynamic.Interface) error {
	existing, err := client.Resource(networks).Get(ctx, "cluster", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		object := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "config.openshift.io/v1",
			"kind":       "Network",
			"metadata":   map[string]any{"name": "cluster"},
			"spec":       networkSpec(),
		}}
		_, err = client.Resource(networks).Create(ctx, object, metav1.CreateOptions{})
		// A create racing the version operator's own delivery of the
		// shell resolves on the next poll's read-and-fill path.
		return err
	}
	if err != nil {
		return err
	}
	if configured, _, _ := unstructured.NestedString(existing.Object, "spec", "networkType"); configured != "" {
		return nil
	}
	if err := unstructured.SetNestedMap(existing.Object, networkSpec(), "spec"); err != nil {
		return err
	}
	_, err = client.Resource(networks).Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

// ensureProbedMTU stores the prober's record of the host MTU. The
// operator's prober job cannot run without nodes, so the seed stands in
// for its result; an existing record is the prober's own output and is
// never overwritten.
func ensureProbedMTU(ctx context.Context, client dynamic.Interface) error {
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": probedMTUName, "namespace": probedMTUNamespace},
		"data":       map[string]any{"mtu": probedMTU},
	}}
	_, err := client.Resource(configMaps).Namespace(probedMTUNamespace).Create(ctx, object, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}
