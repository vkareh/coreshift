// Package bootcache seeds the objects the API's readiness path waits
// on: the OAuth definition the OAuth API server's start gate holds for,
// and the restricted-constraint grant the controller managers' gate
// holds for. They are a different class from the chicken-and-egg seeds
// beside them: those are objects the API server needs before it answers,
// which the version operator can never deliver, while these are ordinary
// payload objects the operator applies anyway. The seed is a cache for
// speed — it applies the same payload manifests the operator applies, so
// the gates pass instantly instead of waiting out the operator's
// convergence, and the operator converges the objects to its own
// versions as it does everything else. Only a release that breaks these
// objects needs a hand update here, and it is caught loudly: the seed
// reads the files the release ships, so one moved or reshaped fails the
// boot instead of drifting silently.
package bootcache

import (
	"context"
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
	"sigs.k8s.io/yaml"

	"github.com/vkareh/coreshift/internal/warnings"
)

// seedFiles lists the payload files the seed applies, relative to the
// payload root the image cache holds: the OAuth definition, and the
// cluster role and binding that let every signed-in user use the
// restricted security context constraint.
var seedFiles = []string{
	"release-manifests/0000_10_config-operator_01_oauths.crd.yaml",
	"release-manifests/0000_20_kube-apiserver-operator_00_cr-scc-restricted-v3.yaml",
	"release-manifests/0000_20_kube-apiserver-operator_00_crb-systemauthenticated-scc-restricted-v3.yaml",
}

// seedResources maps each seeded kind to the resource the API serves it
// under; the dynamic client addresses resources, not kinds. Anything
// else a payload file holds is a seed bug, refused as such.
var seedResources = map[string]schema.GroupVersionResource{
	"CustomResourceDefinition": {Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"},
	"ClusterRole":              {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"},
	"ClusterRoleBinding":       {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"},
}

// objectRetryInterval spaces out retries while the API rejects early
// writes, e.g. until a definition behind them is established. A variable
// for test override.
var objectRetryInterval = 3 * time.Second

// Seed applies the cached objects through the admin kubeconfig and
// waits for the API to serve the definitions among them. The writes are
// retried and idempotent, and objects that already exist — applied by an
// earlier run, or already converged by the version operator — are left
// untouched: the operator owns them.
func Seed(ctx context.Context, kubeconfigPath, payloadDir string) error {
	config, err := warnings.Config(kubeconfigPath)
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
	return waitForEstablished(ctx, client, definitions)
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
			return nil, fmt.Errorf("seed the API's readiness path: %w", ctx.Err())
		}
		return nil, fmt.Errorf("seed the API's readiness path: %w", lastErr)
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
// definition serving requests, so the gate the cache serves passes on an
// API that actually answers.
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
