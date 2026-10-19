// Package crd seeds the custom resource definitions that OpenShift
// controllers expect to already exist when they start. In real deployments
// the config operator applies them; this package does the equivalent for a
// locally run control plane.
package crd

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

// The range allocation definition, copied verbatim from the openshift/api
// repository. The cluster policy controller writes its UID range allocation
// to this API; in a full cluster the config operator applies the definition,
// and nothing else does here.
//
//go:embed 0000_03_config-operator_02_rangeallocations.crd.yaml
var rangeAllocationManifest string

const rangeAllocationName = "rangeallocations.security.internal.openshift.io"

// objectRetryInterval spaces out retries while kube-apiserver warms up or
// rejects early writes. A variable for test override.
var objectRetryInterval = 3 * time.Second

// Seed applies the range allocation definition and waits for the API server
// to serve it, so controllers that write range allocations do not hit a
// missing API. The writes are retried and idempotent, so it is safe to call
// while the API server is still coming up.
func Seed(ctx context.Context, kubeconfigPath string) error {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}
	client, err := apiextensionsclient.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create apiextensions client: %w", err)
	}
	if err := seedWithRetry(ctx, client); err != nil {
		return err
	}
	return waitForEstablished(ctx, client)
}

// seedWithRetry keeps retrying until the context expires. The apiserver
// can reject the first writes while it warms up.
func seedWithRetry(ctx context.Context, client apiextensionsclient.Interface) error {
	var lastErr error
	pollErr := wait.PollUntilContextCancel(ctx, objectRetryInterval, true, func(ctx context.Context) (bool, error) {
		lastErr = seed(ctx, client)
		return lastErr == nil, nil
	})
	if pollErr != nil {
		if lastErr == nil {
			return fmt.Errorf("seed range allocation API: %w", ctx.Err())
		}
		return fmt.Errorf("seed range allocation API: %w", lastErr)
	}
	return nil
}

func seed(ctx context.Context, client apiextensionsclient.Interface) error {
	definition := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal([]byte(rangeAllocationManifest), definition); err != nil {
		return fmt.Errorf("parse %s definition: %w", rangeAllocationName, err)
	}
	// Intentionally left untouched when it already exists, like the
	// permission seeding: nothing about it changes between runs.
	_, err := client.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, definition, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("register %s: %w", rangeAllocationName, err)
	}
	return nil
}

// waitForEstablished polls until the API server reports the definition
// serving requests. Writes against the API are rejected as not found until
// then.
func waitForEstablished(ctx context.Context, client apiextensionsclient.Interface) error {
	pollErr := wait.PollUntilContextCancel(ctx, objectRetryInterval, true, func(ctx context.Context) (bool, error) {
		definition, err := client.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, rangeAllocationName, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		return isEstablished(definition), nil
	})
	if pollErr != nil {
		return fmt.Errorf("wait for %s to be established: %w", rangeAllocationName, ctx.Err())
	}
	return nil
}

func isEstablished(definition *apiextensionsv1.CustomResourceDefinition) bool {
	for _, condition := range definition.Status.Conditions {
		if condition.Type == apiextensionsv1.Established {
			return condition.Status == apiextensionsv1.ConditionTrue
		}
	}
	return false
}
