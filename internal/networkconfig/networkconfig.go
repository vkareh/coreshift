// Package networkconfig seeds the installer's network-relevant cluster
// configuration, the records the cluster network operator reads that
// nothing else delivers. The release payload carries the cluster
// network and infrastructure objects only as create-only empty shells
// — the ranges and the infrastructure record are installer output —
// nothing carries the install config record the operator counts
// control plane replicas from, and nothing carries the probed host MTU,
// which the operator's prober job records from a node this control
// plane does not have. This package stands in for all of them the way
// the pull secret and the OAuth client stand in for theirs: cluster
// inputs, not payload manifests.
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

// The objects the seed writes: the cluster network configuration, the
// cluster's infrastructure record, the install config record in
// kube-system, and in the operator's namespace the record the prober
// leaves behind.
var (
	networks = schema.GroupVersionResource{
		Group:    "config.openshift.io",
		Version:  "v1",
		Resource: "networks",
	}
	infrastructures = schema.GroupVersionResource{
		Group:    "config.openshift.io",
		Version:  "v1",
		Resource: "infrastructures",
	}
	configMaps = schema.GroupVersionResource{
		Version:  "v1",
		Resource: "configmaps",
	}
	probedMTUNamespace = "openshift-network-operator"
	probedMTUName      = "mtu"
	installConfigName  = "cluster-config-v1"
)

// objectRetryInterval spaces out retries while the version operator is
// still delivering the definitions and objects these writes land
// against. A variable for test override.
var objectRetryInterval = 3 * time.Second

// Seed writes the installer's records through the admin kubeconfig:
// the cluster network object with the ranges the control plane runs
// with, the infrastructure record the operator renders by, the install
// config record, and the probed MTU. The writes are retried and
// idempotent, and an object that already carries its record — a warm
// re-boot's — is left exactly as it is.
func Seed(ctx context.Context, kubeconfigPath, apiServerURL, clusterName string) error {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create dynamic client: %w", err)
	}
	return seedWithRetry(ctx, client, apiServerURL, clusterName)
}

// seedWithRetry keeps retrying until the context expires. The version
// operator's delivery decides when the writes can land, so early
// failures are expected and retried.
func seedWithRetry(ctx context.Context, client dynamic.Interface, apiServerURL, clusterName string) error {
	var lastErr error
	pollErr := wait.PollUntilContextCancel(ctx, objectRetryInterval, true, func(ctx context.Context) (bool, error) {
		if lastErr = seed(ctx, client, apiServerURL, clusterName); lastErr != nil {
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

func seed(ctx context.Context, client dynamic.Interface, apiServerURL, clusterName string) error {
	if err := ensureNetwork(ctx, client); err != nil {
		return err
	}
	if err := ensureInfrastructure(ctx, client, apiServerURL, clusterName); err != nil {
		return err
	}
	if err := ensureInstallConfig(ctx, client); err != nil {
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

// infrastructureStatus returns the status the installer writes for a
// default install on no platform: the API endpoint this control plane
// serves as both its public and internal address, the cluster's name
// as its infrastructure name, and the highly available topologies the
// payload's standard manifests assume.
func infrastructureStatus(apiServerURL, clusterName string) map[string]any {
	return map[string]any{
		"infrastructureName":   clusterName,
		"apiServerURL":         apiServerURL,
		"apiServerInternalURI": apiServerURL,
		"platformStatus":       map[string]any{"type": "None"},
		"controlPlaneTopology": "HighlyAvailable",
		"infrastructureTopology": "HighlyAvailable",
	}
}

// ensureInfrastructure fills the cluster's infrastructure record. The
// payload delivers the object as a create-only empty shell and the
// installer owns its status; the operator reads the platform and
// topologies to pick what it renders, so the record must exist before
// it reconciles. The payload's object is waited for, never created, and
// one that already carries a platform — a warm re-boot's — is left
// untouched. The status is written through the status subresource: the
// main resource ignores it.
func ensureInfrastructure(ctx context.Context, client dynamic.Interface, apiServerURL, clusterName string) error {
	existing, err := client.Resource(infrastructures).Get(ctx, "cluster", metav1.GetOptions{})
	if err != nil {
		return err
	}
	if _, found, _ := unstructured.NestedString(existing.Object, "status", "platformStatus", "type"); found {
		return nil
	}
	if err := unstructured.SetNestedMap(existing.Object, infrastructureStatus(apiServerURL, clusterName), "status"); err != nil {
		return err
	}
	_, err = client.Resource(infrastructures).UpdateStatus(ctx, existing, metav1.UpdateOptions{})
	return err
}

// installConfig is the installer's record of the install config, the
// redacted document it stores in kube-system: the cluster's shape as
// this run stands in for it. The operator reads the control plane's
// replica count from it when it renders.
const installConfig = `compute:
- name: worker
  replicas: 0
controlPlane:
  name: master
  replicas: 3
networking:
  clusterNetwork:
  - cidr: 10.128.0.0/14
    hostPrefix: 23
  machineNetwork:
  - cidr: 10.0.0.0/16
  networkType: OVNKubernetes
  serviceNetwork:
  - 172.30.0.0/16
platform:
  none: {}
`

// ensureInstallConfig stores the installer's record of the install
// config. Nothing in the payload carries it; the operator counts the
// control plane's replicas from it, so it must exist before it
// reconciles. An existing record is never overwritten.
func ensureInstallConfig(ctx context.Context, client dynamic.Interface) error {
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": installConfigName, "namespace": "kube-system"},
		"data":       map[string]any{"install-config": installConfig},
	}}
	_, err := client.Resource(configMaps).Namespace("kube-system").Create(ctx, object, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}
