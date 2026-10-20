// Package oauthclient seeds the OAuth clients the login flow needs. In
// real clusters the authentication operator bootstraps them; this package
// does the equivalent for a locally run control plane.
package oauthclient

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

// The client oc logs in with, mirroring the authentication operator's
// bootstrapped challenging client. The redirect URI is the main API
// endpoint's implicit token URL.
//
//go:embed openshift-challenging-client.yaml
var manifest string

// clients is the resource the manifest creates. The API is served by the
// OAuth API server through the aggregation layer, so writes against it
// fail until that server answers.
var clients = schema.GroupVersionResource{
	Group:    "oauth.openshift.io",
	Version:  "v1",
	Resource: "oauthclients",
}

// objectRetryInterval spaces out retries while the aggregated OAuth API
// warms up or rejects early writes. A variable for test override.
var objectRetryInterval = 3 * time.Second

// Seed creates the login clients. The writes are retried and idempotent,
// so it is safe to call while the API is still coming up.
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
// OAuth API can reject the first writes while it warms up.
func seedWithRetry(ctx context.Context, client dynamic.Interface) error {
	object := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(manifest), object); err != nil {
		return fmt.Errorf("parse OAuth client manifest: %w", err)
	}
	var lastErr error
	pollErr := wait.PollUntilContextCancel(ctx, objectRetryInterval, true, func(ctx context.Context) (bool, error) {
		_, lastErr = client.Resource(clients).Create(ctx, object, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(lastErr) {
			lastErr = nil
		}
		return lastErr == nil, nil
	})
	if pollErr != nil {
		if lastErr == nil {
			return fmt.Errorf("seed OAuth clients: %w", ctx.Err())
		}
		return fmt.Errorf("seed OAuth clients: %w", lastErr)
	}
	return nil
}
