// Package scc seeds the default security context constraints that the
// cluster policy controller reconciles. In real deployments the
// kube-apiserver operator applies them from the release payload; this
// package does the equivalent for a locally run control plane.
package scc

import (
	"context"
	"embed"
	"fmt"
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

// The default constraint manifests, copied verbatim from the openshift
// cluster-kube-apiserver-operator repository at the release-4.22 branch.
//
//go:embed 0000_20_kube-apiserver-operator_00_scc-*.yaml
var manifestFS embed.FS

// constraints is the resource the manifests create. The API is served by
// openshift-apiserver through the aggregation layer, so writes against
// it fail until that server answers and the aggregation layer routes to
// it.
var constraints = schema.GroupVersionResource{
	Group:    "security.openshift.io",
	Version:  "v1",
	Resource: "securitycontextconstraints",
}

// objectRetryInterval spaces out retries while the aggregated security
// API warms up or rejects early writes. A variable for test override.
var objectRetryInterval = 3 * time.Second

// Seed creates the default constraints. The writes are retried and
// idempotent, so it is safe to call while the API is still coming up.
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

// seedWithRetry keeps retrying until the context expires. The aggregated
// security API can reject the first writes while it warms up.
func seedWithRetry(ctx context.Context, client dynamic.Interface) error {
	var lastErr error
	pollErr := wait.PollUntilContextCancel(ctx, objectRetryInterval, true, func(ctx context.Context) (bool, error) {
		lastErr = seed(ctx, client)
		return lastErr == nil, nil
	})
	if pollErr != nil {
		if lastErr == nil {
			return fmt.Errorf("seed security context constraints: %w", ctx.Err())
		}
		return fmt.Errorf("seed security context constraints: %w", lastErr)
	}
	return nil
}

func seed(ctx context.Context, client dynamic.Interface) error {
	entries, err := manifestFS.ReadDir(".")
	if err != nil {
		return fmt.Errorf("list constraint manifests: %w", err)
	}
	for _, entry := range entries {
		manifest, err := manifestFS.ReadFile(entry.Name())
		if err != nil {
			return fmt.Errorf("read %s: %w", entry.Name(), err)
		}
		constraint := &unstructured.Unstructured{}
		if err := yaml.Unmarshal(manifest, constraint); err != nil {
			return fmt.Errorf("parse %s: %w", entry.Name(), err)
		}
		// Intentionally left untouched when it already exists, like the
		// permission seeding: nothing about it changes between runs.
		_, err = client.Resource(constraints).Create(ctx, constraint, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("register %s constraint: %w", constraint.GetName(), err)
		}
	}
	return nil
}
