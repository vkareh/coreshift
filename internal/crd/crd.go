// Package crd seeds the custom resource definitions the API server needs
// before it can answer: the quota definition its own admission chain reads
// over every namespaced write, and the role binding restrictions definition
// its own role-binding admission checks before it can bootstrap its
// bindings. The version operator delivers every other definition this
// control plane uses, verified on a fresh boot, so nothing else is seeded
// here.
package crd

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

// The definition manifests, copied verbatim from the openshift/api
// repository. Both must pre-date the API server answering:
//
// The cluster resource quota definition backs the admission chain that
// gates every namespaced write on an informer over the API; without it,
// creating any namespaced object fails. The version operator cannot
// deliver it, because it only applies after the API server answers.
//
// The role binding restrictions definition backs the admission check that
// gates every role binding creation; without it, the API server's own
// bootstrap cannot create its bindings and never becomes healthy. The
// operator does carry this definition, but its delivery lands seconds
// after the API server answers, past the controller manager's bounded
// wait for the API server's health, so a boot relying on it is a coin
// flip the seed removes.
//
//go:embed 0000_00_apiserver_01_clusterresourcequotas.crd.yaml
var clusterResourceQuotaManifest string

//go:embed 0000_03_config-operator_01_rolebindingrestrictions.crd.yaml
var roleBindingRestrictionsManifest string

// definitions pairs each embedded manifest with the name of the definition
// it holds.
var definitions = []struct {
	name     string
	manifest string
}{
	{name: "clusterresourcequotas.quota.openshift.io", manifest: clusterResourceQuotaManifest},
	{name: "rolebindingrestrictions.authorization.openshift.io", manifest: roleBindingRestrictionsManifest},
}

// objectRetryInterval spaces out retries while kube-apiserver warms up or
// rejects early writes. A variable for test override.
var objectRetryInterval = 3 * time.Second

// Seed applies the definitions and waits for the API server to serve them,
// so the API server's own admission has them in place when it answers. The
// writes are retried and idempotent, so it is safe to call while the API
// server is still coming up.
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
			return fmt.Errorf("seed custom resource definitions: %w", ctx.Err())
		}
		return fmt.Errorf("seed custom resource definitions: %w", lastErr)
	}
	return nil
}

func seed(ctx context.Context, client apiextensionsclient.Interface) error {
	for _, def := range definitions {
		definition := &apiextensionsv1.CustomResourceDefinition{}
		if err := yaml.Unmarshal([]byte(def.manifest), definition); err != nil {
			return fmt.Errorf("parse %s definition: %w", def.name, err)
		}
		// Intentionally left untouched when it already exists, like the
		// permission seeding: nothing about it changes between runs.
		_, err := client.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, definition, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("register %s: %w", def.name, err)
		}
	}
	return nil
}

// waitForEstablished polls until the API server reports every definition
// serving requests. Writes against an API are rejected as not found until
// then.
func waitForEstablished(ctx context.Context, client apiextensionsclient.Interface) error {
	for _, def := range definitions {
		pollErr := wait.PollUntilContextCancel(ctx, objectRetryInterval, true, func(ctx context.Context) (bool, error) {
			definition, err := client.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, def.name, metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			return isEstablished(definition), nil
		})
		if pollErr != nil {
			return fmt.Errorf("wait for %s to be established: %w", def.name, ctx.Err())
		}
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
