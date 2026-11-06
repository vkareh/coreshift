package bootcache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

// newClient returns a fake serving the resources the seed writes.
func newClient() *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}: "CustomResourceDefinitionList",
			{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}:         "ClusterRoleList",
			{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}:  "ClusterRoleBindingList",
		},
	)
}

// writeTestPayload writes the seed files with the shapes the payload
// gives them: the OAuth definition, and the restricted-constraint grant.
func writeTestPayload(t *testing.T) string {
	t.Helper()
	payload := t.TempDir()
	files := map[string]string{
		"release-manifests/0000_10_config-operator_01_oauths.crd.yaml": `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: oauths.config.openshift.io
`,
		"release-manifests/0000_20_kube-apiserver-operator_00_cr-scc-restricted-v3.yaml": `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: system:openshift:scc:restricted-v3
rules:
- apiGroups:
  - security.openshift.io
  resourceNames:
  - restricted-v3
  resources:
  - securitycontextconstraints
  verbs:
  - use
`,
		"release-manifests/0000_20_kube-apiserver-operator_00_crb-systemauthenticated-scc-restricted-v3.yaml": `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: system:openshift:scc:restricted-v3
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:openshift:scc:restricted-v3
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: Group
  name: system:authenticated
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

// shortenRetries makes the seed retry in milliseconds for the duration
// of a test.
func shortenRetries(t *testing.T) {
	t.Helper()
	objectRetryInterval = time.Millisecond
	t.Cleanup(func() { objectRetryInterval = 3 * time.Second })
}

// TestSeedAppliesTheCachedObjects checks the OAuth definition and the
// restricted-constraint grant all exist after the seed.
func TestSeedAppliesTheCachedObjects(t *testing.T) {
	shortenRetries(t)

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

	for name, resource := range map[schema.GroupVersionResource]string{
		seedResources["CustomResourceDefinition"]: "oauths.config.openshift.io",
		seedResources["ClusterRole"]:              "system:openshift:scc:restricted-v3",
		seedResources["ClusterRoleBinding"]:       "system:openshift:scc:restricted-v3",
	} {
		if _, err := client.Resource(name).Get(context.Background(), resource, metav1.GetOptions{}); err != nil {
			t.Fatalf("%s was not created: %v", resource, err)
		}
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
}

// TestSeedRetriesRejectedWrites checks the seed survives the API
// refusing writes while it warms up.
func TestSeedRetriesRejectedWrites(t *testing.T) {
	shortenRetries(t)

	client := newClient()
	var calls int
	client.PrependReactor("create", "clusterroles", func(action ktesting.Action) (bool, runtime.Object, error) {
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
	rogue := filepath.Join(payload, "release-manifests/0000_10_config-operator_01_oauths.crd.yaml")
	if err := os.WriteFile(rogue, []byte("apiVersion: v1\nkind: Secret\nmetadata:\n  name: rogue\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := seed(context.Background(), newClient(), payload); err == nil {
		t.Fatal("expected error for unknown kind")
	}
}

// TestSeedRefusesMissingFile checks a release that moved a seeded file
// fails the boot loudly instead of skipping the cache.
func TestSeedRefusesMissingFile(t *testing.T) {
	payload := writeTestPayload(t)
	if err := os.Remove(filepath.Join(payload, seedFiles[0])); err != nil {
		t.Fatal(err)
	}

	_, err := seed(context.Background(), newClient(), payload)
	if err == nil || !strings.Contains(err.Error(), seedFiles[0]) {
		t.Fatalf("error = %v; want the missing payload file named", err)
	}
}
