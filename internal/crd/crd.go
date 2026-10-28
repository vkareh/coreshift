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
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

// The definition manifests, copied verbatim from the openshift/api
// repository. In a full cluster the config operator applies them, and
// nothing else does here.
//
// The cluster resource quota definition backs the admission chain that
// gates every namespaced write on an informer over the API; without it,
// creating any namespaced object fails.
//
//go:embed 0000_00_apiserver_01_clusterresourcequotas.crd.yaml
var clusterResourceQuotaManifest string

// The security context constraints definition backs the objects the
// cluster policy controller reconciles; without it, listing the
// constraints fails.
//
//go:embed 0000_03_config-operator_01_securitycontextconstraints.crd.yaml
var securityContextConstraintsManifest string

// The range allocation definition backs the UID range allocation the
// cluster policy controller writes.
//
//go:embed 0000_03_config-operator_02_rangeallocations.crd.yaml
var rangeAllocationManifest string

// The role binding restrictions definition backs the admission check that
// gates every role binding creation; without it, the RBAC bootstrap cannot
// create its bindings and the API server never becomes healthy.
//
//go:embed 0000_03_config-operator_01_rolebindingrestrictions.crd.yaml
var roleBindingRestrictionsManifest string

// The OAuth definition backs the OAuth objects the OAuth server watches to
// load identity providers at runtime; without it, applying one fails with
// "no matches for kind OAuth".
//
//go:embed 0000_10_config-operator_01_oauths.crd.yaml
var oauthManifest string

// The Infrastructure definition backs the cluster's Infrastructure object,
// which the managed-node admission reads before permitting node creation;
// without it, no node can register.
//
//go:embed 0000_10_config-operator_01_infrastructures-Default.crd.yaml
var infrastructureManifest string

// The Machine definition backs the machine objects the machine approver
// matches node certificate requests against; without it, the approver never
// sees a machine and approves nothing.
//
//go:embed 0000_30_machine-api-operator_02_machine.Default.crd.yaml
var machineManifest string

// definitions pairs each embedded manifest with the name of the definition
// it holds.
var definitions = []struct {
	name     string
	manifest string
}{
	{name: "clusterresourcequotas.quota.openshift.io", manifest: clusterResourceQuotaManifest},
	{name: "securitycontextconstraints.security.openshift.io", manifest: securityContextConstraintsManifest},
	{name: "rangeallocations.security.internal.openshift.io", manifest: rangeAllocationManifest},
	{name: "rolebindingrestrictions.authorization.openshift.io", manifest: roleBindingRestrictionsManifest},
	{name: "oauths.config.openshift.io", manifest: oauthManifest},
	{name: "infrastructures.config.openshift.io", manifest: infrastructureManifest},
	{name: "machines.machine.openshift.io", manifest: machineManifest},
}

// objectRetryInterval spaces out retries while kube-apiserver warms up or
// rejects early writes. A variable for test override.
var objectRetryInterval = 3 * time.Second

// Seed applies the definitions and waits for the API server to serve them,
// so controllers that call these APIs do not hit a missing one. The writes
// are retried and idempotent, so it is safe to call while the API server is
// still coming up.
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
