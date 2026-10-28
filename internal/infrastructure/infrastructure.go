// Package infrastructure seeds the cluster's Infrastructure object. In
// real deployments the installer applies it and the config operator keeps
// it; this package does the equivalent for a locally run control plane,
// applying the payload's own minimal platform-less object, the base every
// cluster starts from.
package infrastructure

import (
	"context"
	_ "embed"
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

// The cluster's Infrastructure object, copied verbatim from the payload's
// config manifests. The managed-node admission reads it before permitting
// node creation; the payload ships exactly this minimal platform-less
// shape, which the installer fills in on real clusters.
//
//go:embed 0000_05_config-operator_02_infrastructure.cr.yaml
var manifest string

// objects is the resource the manifest creates. The API is served by
// kube-apiserver itself through the Infrastructure definition, so writes
// against it fail until that definition is established.
var objects = schema.GroupVersionResource{
	Group:    "config.openshift.io",
	Version:  "v1",
	Resource: "infrastructures",
}

// objectRetryInterval spaces out retries while the definition settles or
// kube-apiserver rejects early writes. A variable for test override.
var objectRetryInterval = 3 * time.Second

// Seed creates the Infrastructure object. The writes are retried and
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

// seedWithRetry keeps retrying until the context expires. The API can
// reject the first writes while the definition behind it settles.
func seedWithRetry(ctx context.Context, client dynamic.Interface) error {
	object := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(manifest), object); err != nil {
		return fmt.Errorf("parse Infrastructure manifest: %w", err)
	}
	var lastErr error
	pollErr := wait.PollUntilContextCancel(ctx, objectRetryInterval, true, func(ctx context.Context) (bool, error) {
		_, lastErr = client.Resource(objects).Create(ctx, object, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(lastErr) {
			lastErr = nil
		}
		return lastErr == nil, nil
	})
	if pollErr != nil {
		if lastErr == nil {
			return fmt.Errorf("seed Infrastructure object: %w", ctx.Err())
		}
		return fmt.Errorf("seed Infrastructure object: %w", lastErr)
	}
	return nil
}
