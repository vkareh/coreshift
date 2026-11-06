package clusterversion

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"
)

// newClient returns a fake serving the resources the seed writes.
func newClient() *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			{Version: "v1", Resource: "namespaces"}:                                               "NamespaceList",
			{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}: "CustomResourceDefinitionList",
			{Group: "config.openshift.io", Version: "v1", Resource: "featuregates"}:               "FeatureGateList",
			{Group: "config.openshift.io", Version: "v1", Resource: "clusterversions"}:            "ClusterVersionList",
		},
	)
}

// writeTestPayload writes the seed files with the shapes the payload
// gives them, one definition per config API.
func writeTestPayload(t *testing.T) string {
	t.Helper()
	payload := t.TempDir()
	files := map[string]string{
		"manifests/0000_00_cluster-version-operator_00_namespace.yaml": `apiVersion: v1
kind: Namespace
metadata:
  name: openshift-cluster-version
`,
		"manifests/0000_00_cluster-version-operator_01_clusterversions-Default.crd.yaml": `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: clusterversions.config.openshift.io
`,
		"manifests/0000_00_cluster-version-operator_01_clusteroperators.crd.yaml": `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: clusteroperators.config.openshift.io
`,
		"release-manifests/0000_03_config-operator_01_proxies.crd.yaml": `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: proxies.config.openshift.io
`,
		"release-manifests/0000_10_config-operator_01_apiservers-Default.crd.yaml": `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: apiservers.config.openshift.io
`,
		"release-manifests/0000_10_config-operator_01_featuregates.crd.yaml": `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: featuregates.config.openshift.io
`,
		"release-manifests/0000_05_config-operator_02_featuregate.cr.yaml": `apiVersion: config.openshift.io/v1
kind: FeatureGate
metadata:
  name: cluster
spec: {}
`,
	}
	for name, manifest := range files {
		path := filepath.Join(payload, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return payload
}

// markEstablished answers definition reads the way the API server does
// once its definition controller has run, after one unanswered read.
func markEstablished(client *dynamicfake.FakeDynamicClient) {
	gets := map[string]int{}
	client.PrependReactor("get", "customresourcedefinitions", func(action ktesting.Action) (bool, runtime.Object, error) {
		name := action.(ktesting.GetAction).GetName()
		gets[name]++
		definition := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "apiextensions.k8s.io/v1",
			"kind":       "CustomResourceDefinition",
			"metadata":   map[string]any{"name": name},
		}}
		if gets[name] > 1 {
			definition.Object["status"] = map[string]any{
				"conditions": []any{map[string]any{"type": "Established", "status": "True"}},
			}
		}
		return true, definition, nil
	})
}

// TestSeedAppliesPrerequisites checks the operator's namespace, the
// definitions its startup reads, and the object it waits for all exist
// after the seed.
func TestSeedAppliesPrerequisites(t *testing.T) {
	objectRetryInterval = time.Millisecond
	t.Cleanup(func() { objectRetryInterval = 3 * time.Second })

	client := newClient()
	payload := writeTestPayload(t)

	definitions, err := seedWithRetry(context.Background(), client, payload)
	if err != nil {
		t.Fatal(err)
	}
	markEstablished(client)
	if err := waitForEstablished(context.Background(), client, definitions); err != nil {
		t.Fatal(err)
	}
	if err := ensureObject(context.Background(), client); err != nil {
		t.Fatal(err)
	}

	if _, err := client.Resource(seedResources["Namespace"]).Get(context.Background(), seedNamespace, metav1.GetOptions{}); err != nil {
		t.Fatalf("the %s namespace was not created: %v", seedNamespace, err)
	}
	if _, err := client.Resource(seedResources["FeatureGate"]).Get(context.Background(), "cluster", metav1.GetOptions{}); err != nil {
		t.Fatalf("the cluster FeatureGate was not created: %v", err)
	}
	for _, name := range definitions {
		if _, err := client.Resource(seedResources["CustomResourceDefinition"]).Get(context.Background(), name, metav1.GetOptions{}); err != nil {
			t.Fatalf("%s was not created: %v", name, err)
		}
	}
	object, err := client.Resource(objects).Get(context.Background(), objectName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the %s ClusterVersion was not created: %v", objectName, err)
	}
	clusterID, found, err := unstructured.NestedString(object.Object, "spec", "clusterID")
	if err != nil || !found {
		t.Fatalf("object clusterID = %q, %v; want the identity the API requires", clusterID, err)
	}
	if len(clusterID) != 36 {
		t.Fatalf("object clusterID = %q, want a UUID", clusterID)
	}
}

// TestSeedIsRepeatable checks a warm boot: everything the seed applies
// already exists, and the seed leaves it alone.
func TestSeedIsRepeatable(t *testing.T) {
	client := newClient()
	payload := writeTestPayload(t)

	for range 2 {
		if _, err := seedWithRetry(context.Background(), client, payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := ensureObject(context.Background(), client); err != nil {
		t.Fatal(err)
	}
}

// TestEnsureObjectWritesOnlyTheAdoptionSpec checks the object the seed
// creates carries nothing but the cluster identity: the operator owns
// the object from its first boot, writing its status, history, and
// desired state itself, so the run's whole contribution is the spec
// that hands the cluster over.
func TestEnsureObjectWritesOnlyTheAdoptionSpec(t *testing.T) {
	client := newClient()

	if err := ensureObject(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	object, err := client.Resource(objects).Get(context.Background(), objectName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	spec, found, err := unstructured.NestedMap(object.Object, "spec")
	if err != nil || !found || len(spec) != 1 {
		t.Fatalf("object spec = %v, %v; want only the cluster identity", spec, err)
	}
	if _, hasID := spec["clusterID"]; !hasID {
		t.Fatalf("object spec = %v, want the cluster identity", spec)
	}
	if _, found, _ := unstructured.NestedMap(object.Object, "status"); found {
		t.Fatal("object carries a status; the operator owns the status")
	}
}

// TestEnsureObjectLeavesExistingStatus checks the object the operator
// already reported on survives a re-boot untouched, because the
// operator owns it from its first boot.
func TestEnsureObjectLeavesExistingStatus(t *testing.T) {
	client := newClient()
	reported := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(`apiVersion: config.openshift.io/v1
kind: ClusterVersion
metadata:
  name: version
status:
  desired:
    image: quay.io/example/release:v1
`), reported); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Resource(objects).Create(context.Background(), reported, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := ensureObject(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	object, err := client.Resource(objects).Get(context.Background(), objectName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	status, found, err := unstructured.NestedMap(object.Object, "status")
	if err != nil || !found {
		t.Fatalf("object status = %v, %v; want the reported one kept", status, err)
	}
	if image, _, _ := unstructured.NestedString(status, "desired", "image"); image != "quay.io/example/release:v1" {
		t.Fatalf("status image = %q, want the reported one kept", image)
	}
}

// TestSeedRetriesRejectedWrites checks the seed survives the API
// refusing writes while it warms up.
func TestSeedRetriesRejectedWrites(t *testing.T) {
	objectRetryInterval = time.Millisecond
	t.Cleanup(func() { objectRetryInterval = 3 * time.Second })

	client := newClient()
	var calls int
	client.PrependReactor("create", "customresourcedefinitions", func(action ktesting.Action) (bool, runtime.Object, error) {
		calls++
		if calls == 1 {
			return true, nil, apierrors.NewInternalError(errors.New("etcdserver: request timed out"))
		}
		return false, nil, nil
	})

	if _, err := seedWithRetry(context.Background(), client, writeTestPayload(t)); err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Fatalf("rejected write was not retried, calls: %d", calls)
	}
}

// TestSeedRefusesUnknownKind checks a payload file holding anything but
// the seeded kinds is a loud failure, not a silent skip.
func TestSeedRefusesUnknownKind(t *testing.T) {
	payload := writeTestPayload(t)
	rogue := filepath.Join(payload, "manifests/0000_00_cluster-version-operator_01_clusterversions-Default.crd.yaml")
	if err := os.WriteFile(rogue, []byte("apiVersion: v1\nkind: Secret\nmetadata:\n  name: rogue\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := seed(context.Background(), newClient(), payload); err == nil {
		t.Fatal("expected error for unknown kind")
	}
}

// TestWaitForEstablishedWaitsForTheAPI checks the wait polls until the
// definitions report serving.
func TestWaitForEstablishedWaitsForTheAPI(t *testing.T) {
	objectRetryInterval = time.Millisecond
	t.Cleanup(func() { objectRetryInterval = 3 * time.Second })

	client := newClient()
	markEstablished(client)

	if err := waitForEstablished(context.Background(), client, []string{"clusterversions.config.openshift.io"}); err != nil {
		t.Fatal(err)
	}
}
