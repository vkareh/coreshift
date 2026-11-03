package featuregates

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/yaml"
)

// newClient returns a fake serving the resources the seed reads and
// writes, with any objects already in place.
func newClient(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			{Group: "config.openshift.io", Version: "v1", Resource: "featuregates"}:  "FeatureGateList",
			{Group: "config.openshift.io", Version: "v1", Resource: "clusterversions"}: "ClusterVersionList",
		},
		objects...,
	)
}

// object returns a parsed manifest, for objects that already exist.
func object(t *testing.T, manifest string) *unstructured.Unstructured {
	t.Helper()
	parsed := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(manifest), parsed); err != nil {
		t.Fatal(err)
	}
	return parsed
}

// writeFeatureSetWriter writes a stand-in for the release's feature
// set writer: a script that copies the rendered manifests it is given
// into the output directory the caller names.
func writeFeatureSetWriter(t *testing.T, rendered string) string {
	t.Helper()
	writer := filepath.Join(t.TempDir(), "write-available-featuresets")
	script := fmt.Sprintf("#!/bin/bash\ndir=\"${1#--asset-output-dir=}\"\ncp %q/* \"$dir\"\n", rendered)
	if err := os.WriteFile(writer, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return writer
}

// writeRenderedFeatureSets writes the manifests the release's writer
// renders for one version: the default set, a second set, and another
// profile's default the seed must ignore.
func writeRenderedFeatureSets(t *testing.T, version string) string {
	t.Helper()
	rendered := t.TempDir()
	files := map[string]string{
		"featureGate-4-SelfManagedHA-Default.yaml": fmt.Sprintf(`{
			"apiVersion": "config.openshift.io/v1",
			"kind": "FeatureGate",
			"metadata": {"name": "cluster"},
			"spec": {},
			"status": {"featureGates": [{"version": %q,
				"enabled": [{"name": "AlwaysOn"}, {"name": "BothWays"}],
				"disabled": [{"name": "AlwaysOff"}]}]}
		}`, version),
		"featureGate-4-SelfManagedHA-LatencySensitive.yaml": fmt.Sprintf(`{
			"apiVersion": "config.openshift.io/v1",
			"kind": "FeatureGate",
			"metadata": {"name": "cluster"},
			"spec": {"featureSet": "LatencySensitive"},
			"status": {"featureGates": [{"version": %q,
				"enabled": [{"name": "AlwaysOn"}, {"name": "FastButRisky"}],
				"disabled": [{"name": "AlwaysOff"}]}]}
		}`, version),
		"featureGate-4-Hypershift-Default.yaml": `{
			"apiVersion": "config.openshift.io/v1",
			"kind": "FeatureGate",
			"metadata": {"name": "cluster"},
			"spec": {},
			"status": {"featureGates": [{"version": "0.0.1-snapshot",
				"enabled": [{"name": "HypershiftOnly"}],
				"disabled": []}]}
		}`,
	}
	for name, manifest := range files {
		if err := os.WriteFile(filepath.Join(rendered, name), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return rendered
}

// writePayload writes the payload's version record.
func writePayload(t *testing.T, version string) string {
	t.Helper()
	payload := t.TempDir()
	metadata := filepath.Join(payload, "release-manifests", "release-metadata")
	if err := os.MkdirAll(filepath.Dir(metadata), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadata, []byte(fmt.Sprintf(`{"version": %q}`, version)), 0o644); err != nil {
		t.Fatal(err)
	}
	return payload
}

// recordedEntry returns the entry recorded for a version, or nil.
func recordedEntry(t *testing.T, client *dynamicfake.FakeDynamicClient, version string) map[string]any {
	t.Helper()
	object, err := client.Resource(featureGates).Get(context.Background(), "cluster", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the cluster FeatureGate: %v", err)
	}
	entries, found, _ := unstructured.NestedSlice(object.Object, "status", "featureGates")
	if !found {
		return nil
	}
	for _, entry := range entries {
		values, _ := entry.(map[string]any)
		if values["version"] == version {
			return values
		}
	}
	return nil
}

// entryNames returns the feature names an entry records under one list.
func entryNames(t *testing.T, entry map[string]any, list string) []string {
	t.Helper()
	items, _, _ := unstructured.NestedSlice(entry, list)
	var names []string
	for _, item := range items {
		values, _ := item.(map[string]any)
		names = append(names, values["name"].(string))
	}
	return names
}

// TestSeedRecordsTheCompletedDefaultSet checks the entry the seed
// writes: the default set's enabled features, and every feature the
// release knows that the set does not enable, explicitly disabled.
func TestSeedRecordsTheCompletedDefaultSet(t *testing.T) {
	client := newClient(object(t, `apiVersion: config.openshift.io/v1
kind: FeatureGate
metadata:
  name: cluster
spec: {}
`), object(t, `apiVersion: config.openshift.io/v1
kind: ClusterVersion
metadata:
  name: version
spec:
  clusterID: 00000000-0000-0000-0000-000000000000
`))
	writer := writeFeatureSetWriter(t, writeRenderedFeatureSets(t, "4.22.21"))
	payload := writePayload(t, "4.22.21")

	if err := seed(context.Background(), client, writer, payload); err != nil {
		t.Fatal(err)
	}

	entry := recordedEntry(t, client, "4.22.21")
	if entry == nil {
		t.Fatal("no entry recorded for the release's version")
	}
	if got := entryNames(t, entry, "enabled"); len(got) != 2 || got[0] != "AlwaysOn" || got[1] != "BothWays" {
		t.Fatalf("enabled = %v, want the default set's enabled features", got)
	}
	// FastButRisky is enabled only by the other set the release
	// renders, so the completed default set disables it.
	got := entryNames(t, entry, "disabled")
	if len(got) != 2 || got[0] != "AlwaysOff" || got[1] != "FastButRisky" {
		t.Fatalf("disabled = %v, want the default set's disabled features and the release's other features", got)
	}
}

// TestSeedKeepsHistoryEntries checks the entries of versions the
// cluster's history records stay, and obsolete ones go.
func TestSeedKeepsHistoryEntries(t *testing.T) {
	client := newClient(object(t, `apiVersion: config.openshift.io/v1
kind: FeatureGate
metadata:
  name: cluster
spec: {}
status:
  featureGates:
  - version: 4.21.10
    enabled:
    - name: OldFeature
    disabled: []
  - version: 4.20.0
    enabled:
    - name: OlderFeature
    disabled: []
`), object(t, `apiVersion: config.openshift.io/v1
kind: ClusterVersion
metadata:
  name: version
spec:
  clusterID: 00000000-0000-0000-0000-000000000000
status:
  history:
  - version: 4.22.21
  - version: 4.21.10
`))
	writer := writeFeatureSetWriter(t, writeRenderedFeatureSets(t, "4.22.21"))
	payload := writePayload(t, "4.22.21")

	if err := seed(context.Background(), client, writer, payload); err != nil {
		t.Fatal(err)
	}

	if entry := recordedEntry(t, client, "4.21.10"); entry == nil {
		t.Fatal("the history version's entry was dropped")
	}
	if entry := recordedEntry(t, client, "4.20.0"); entry != nil {
		t.Fatal("the obsolete version's entry was kept")
	}
}

// TestSeedReportsWriterFailure checks a writer that fails surfaces
// with its output.
func TestSeedReportsWriterFailure(t *testing.T) {
	writer := filepath.Join(t.TempDir(), "failing-writer")
	if err := os.WriteFile(writer, []byte("#!/bin/bash\necho cannot render >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	err := Seed(context.Background(), "", writer, writePayload(t, "4.22.21"))
	if err == nil {
		t.Fatal("expected the writer's failure to surface")
	}
	for _, want := range []string{"render the release's feature sets", "cannot render"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want it to name %q", err, want)
		}
	}
}
