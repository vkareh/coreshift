// Package clusterversion seeds what the cluster-version operator needs
// before it can start: the config API definitions its startup informers
// read, the namespace it elects its leader in, and the ClusterVersion
// object it waits for and then owns. The installer's bootstrap applies
// the same set before the operator's bootstrap pod runs; this package
// does the equivalent for a locally run control plane, reading the
// definitions from the release payload so they match the release the
// operator applies.
package clusterversion

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

// The operator's place and object, the defaults it runs with: the
// namespace of its lease and the singleton it reconciles.
const (
	seedNamespace = "openshift-cluster-version"
	objectName    = "version"
)

// seedFiles lists the payload files the seed applies, relative to the
// payload root the image cache holds. The operator cannot apply these
// itself: it reads the config APIs at startup and waits for the
// ClusterVersion object before applying anything, so exactly this set
// must pre-date it. The ClusterVersion definition is the Default
// variant, the one the self-managed-high-availability profile the
// operator runs under includes.
var seedFiles = []string{
	"manifests/0000_00_cluster-version-operator_00_namespace.yaml",
	"manifests/0000_00_cluster-version-operator_01_clusterversions-Default.crd.yaml",
	"manifests/0000_00_cluster-version-operator_01_clusteroperators.crd.yaml",
	"release-manifests/0000_03_config-operator_01_proxies.crd.yaml",
	"release-manifests/0000_10_config-operator_01_featuregates.crd.yaml",
}

// seedResources maps each seeded kind to the resource the API serves it
// under; the dynamic client addresses resources, not kinds. Anything
// else a payload file holds is a seed bug, refused as such.
var seedResources = map[string]schema.GroupVersionResource{
	"Namespace":                {Version: "v1", Resource: "namespaces"},
	"CustomResourceDefinition": {Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"},
}

// The ClusterVersion object the operator waits for before it loads its
// payload. The installer writes the same object for the release it
// installs; the spec stays empty because the operator takes the release
// image from its start flags and fills in the status itself.
//
//go:embed clusterversion.yaml
var manifest string

// objects is the resource the manifest creates.
var objects = schema.GroupVersionResource{
	Group:    "config.openshift.io",
	Version:  "v1",
	Resource: "clusterversions",
}

// objectRetryInterval spaces out retries while the API rejects early
// writes, e.g. until a definition behind them is established. A variable
// for test override.
var objectRetryInterval = 3 * time.Second

// Seed applies the operator's prerequisites through the admin kubeconfig
// and waits for the API to serve them, so the operator finds them in
// place when it starts. The writes are retried and idempotent. Objects
// the operator or an earlier run created are left untouched: the operator
// owns them from its first boot.
func Seed(ctx context.Context, kubeconfigPath, payloadDir string) error {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create dynamic client: %w", err)
	}
	definitions, err := seedWithRetry(ctx, client, payloadDir)
	if err != nil {
		return err
	}
	if err := waitForEstablished(ctx, client, definitions); err != nil {
		return err
	}
	return ensureObject(ctx, client)
}

// seedWithRetry keeps retrying until the context expires. The API can
// reject the first writes while it warms up.
func seedWithRetry(ctx context.Context, client dynamic.Interface, payloadDir string) ([]string, error) {
	var definitions []string
	var lastErr error
	pollErr := wait.PollUntilContextCancel(ctx, objectRetryInterval, true, func(ctx context.Context) (bool, error) {
		definitions, lastErr = seed(ctx, client, payloadDir)
		return lastErr == nil, nil
	})
	if pollErr != nil {
		if lastErr == nil {
			return nil, fmt.Errorf("seed the operator's prerequisites: %w", ctx.Err())
		}
		return nil, fmt.Errorf("seed the operator's prerequisites: %w", lastErr)
	}
	return definitions, nil
}

// seed applies each seed file's object, returning the names of the
// definitions it applied.
func seed(ctx context.Context, client dynamic.Interface, payloadDir string) ([]string, error) {
	var definitions []string
	for _, name := range seedFiles {
		object, err := readObject(payloadDir, name)
		if err != nil {
			return nil, err
		}
		if object.GetKind() == "CustomResourceDefinition" {
			definitions = append(definitions, object.GetName())
		}
		resource, err := resourceFor(object)
		if err != nil {
			return nil, err
		}
		if err := create(ctx, client, resource, object); err != nil {
			return nil, err
		}
	}
	return definitions, nil
}

// readObject reads and parses one payload manifest.
func readObject(payloadDir, name string) (*unstructured.Unstructured, error) {
	data, err := os.ReadFile(filepath.Join(payloadDir, name))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	object := &unstructured.Unstructured{}
	if err := yaml.Unmarshal(data, object); err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	return object, nil
}

// create stores one object, tolerating one that already exists.
func create(ctx context.Context, client dynamic.Interface, resource schema.GroupVersionResource, object *unstructured.Unstructured) error {
	_, err := client.Resource(resource).Create(ctx, object, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("create %s: %w", object.GetName(), err)
	}
	return nil
}

// resourceFor returns the resource the object's kind is served under,
// for the kinds the seed applies.
func resourceFor(object *unstructured.Unstructured) (schema.GroupVersionResource, error) {
	resource, ok := seedResources[object.GetKind()]
	if !ok {
		return schema.GroupVersionResource{}, fmt.Errorf("%s is not a kind the seed applies", object.GroupVersionKind())
	}
	return resource, nil
}

// waitForEstablished polls until the API server reports every seeded
// definition serving requests. Writes against an API are rejected as
// not found until then.
func waitForEstablished(ctx context.Context, client dynamic.Interface, definitions []string) error {
	resource := seedResources["CustomResourceDefinition"]
	for _, name := range definitions {
		pollErr := wait.PollUntilContextCancel(ctx, objectRetryInterval, true, func(ctx context.Context) (bool, error) {
			definition, err := client.Resource(resource).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			return isEstablished(definition), nil
		})
		if pollErr != nil {
			return fmt.Errorf("wait for %s to be established: %w", name, ctx.Err())
		}
	}
	return nil
}

// isEstablished reports whether a definition, read as unstructured data,
// carries the API's established condition.
func isEstablished(definition *unstructured.Unstructured) bool {
	conditions, found, err := unstructured.NestedSlice(definition.Object, "status", "conditions")
	if !found || err != nil {
		return false
	}
	for _, condition := range conditions {
		values, ok := condition.(map[string]any)
		if ok && values["type"] == "Established" && values["status"] == "True" {
			return true
		}
	}
	return false
}

// ensureObject creates the ClusterVersion the operator waits for. The
// operator writes the object's status from its first boot, so an
// existing object is left exactly as it is.
func ensureObject(ctx context.Context, client dynamic.Interface) error {
	object := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(manifest), object); err != nil {
		return fmt.Errorf("parse ClusterVersion manifest: %w", err)
	}
	if err := create(ctx, client, objects, object); err != nil {
		return fmt.Errorf("create the %s ClusterVersion: %w", objectName, err)
	}
	return nil
}
