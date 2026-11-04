// Package featuregates records the release's feature gates on the
// cluster's FeatureGate object, standing in for the config operator:
// the operand that maintains the record in a real cluster runs as a
// payload Deployment and sits unscheduled here. The record is rendered
// by the release's own feature set writer, so what lands is the
// payload's own truth, never invented. The network operator's feature
// gate reader requires the release's entry — its wait is not tolerant
// like the version operator's — and the version operator's
// stop-on-change controller requires the record to pre-date its start,
// so the seed runs before the version operator does.
package featuregates

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"

	"github.com/vkareh/coreshift/internal/warnings"
	"github.com/vkareh/coreshift/pkg/image"
)

// The resources the seed reads and writes: the cluster's feature gate
// record and the cluster version whose history the record keeps
// entries for.
var (
	featureGates = schema.GroupVersionResource{
		Group:    "config.openshift.io",
		Version:  "v1",
		Resource: "featuregates",
	}
	clusterVersions = schema.GroupVersionResource{
		Group:    "config.openshift.io",
		Version:  "v1",
		Resource: "clusterversions",
	}
)

// featureSet is one rendered feature set: the feature names it enables
// and disables for the release's version.
type featureSet struct {
	enabled  []string
	disabled []string
}

// Seed renders the release's feature gate sets with the release's own
// writer and records the default set's completed entry for the
// release's version on the FeatureGate object. Entries for versions in
// the cluster's history are kept, the way the config operator keeps
// them, so an in-place upgrade's re-boot preserves the older record.
func Seed(ctx context.Context, kubeconfigPath, writerPath, payloadDir string) error {
	version, err := image.PayloadVersion(payloadDir)
	if err != nil {
		return err
	}
	sets, err := renderFeatureSets(ctx, writerPath, version)
	if err != nil {
		return err
	}
	config, err := warnings.Config(kubeconfigPath)
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create dynamic client: %w", err)
	}
	return record(ctx, client, sets, version)
}

// seed renders the sets and records them against an existing client.
func seed(ctx context.Context, client dynamic.Interface, writerPath, payloadDir string) error {
	version, err := image.PayloadVersion(payloadDir)
	if err != nil {
		return err
	}
	sets, err := renderFeatureSets(ctx, writerPath, version)
	if err != nil {
		return err
	}
	return record(ctx, client, sets, version)
}

// renderFeatureSets runs the release's feature set writer for the
// release's version and returns the feature sets it rendered. The
// rendered files carry every cluster profile the release supports;
// this control plane runs the self-managed profile, so the other
// profiles' files are ignored the way the config operator ignores
// them.
func renderFeatureSets(ctx context.Context, writerPath, version string) (map[string]featureSet, error) {
	dir, err := os.MkdirTemp("", "coreshift-featuregates-")
	if err != nil {
		return nil, fmt.Errorf("create render directory: %w", err)
	}
	defer os.RemoveAll(dir)

	rendered := exec.CommandContext(ctx, writerPath, "--asset-output-dir="+dir, "--payload-version="+version)
	if out, err := rendered.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("render the release's feature sets: %w\n%s", err, out)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	sets := map[string]featureSet{}
	for _, entry := range entries {
		if !strings.Contains(entry.Name(), "-SelfManagedHA-") {
			continue
		}
		set, err := readFeatureSet(filepath.Join(dir, entry.Name()), version)
		if err != nil {
			return nil, err
		}
		sets[set.name] = featureSet{enabled: set.enabled, disabled: set.disabled}
	}
	if _, ok := sets[""]; !ok {
		return nil, fmt.Errorf("the release renders no default feature set")
	}
	return sets, nil
}

// renderedFeatureSet is one rendered manifest: the feature set it
// describes and that set's entries for the release's version.
type renderedFeatureSet struct {
	name     string
	enabled  []string
	disabled []string
}

// readFeatureSet reads one rendered manifest.
func readFeatureSet(path, version string) (renderedFeatureSet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return renderedFeatureSet{}, err
	}
	manifest := &unstructured.Unstructured{}
	if err := yaml.Unmarshal(data, manifest); err != nil {
		return renderedFeatureSet{}, fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}
	name, _, _ := unstructured.NestedString(manifest.Object, "spec", "featureSet")
	entries, found, err := unstructured.NestedSlice(manifest.Object, "status", "featureGates")
	if err != nil || !found {
		return renderedFeatureSet{}, fmt.Errorf("%s renders no feature gate entries: %v", filepath.Base(path), err)
	}
	for _, entry := range entries {
		values, _ := entry.(map[string]any)
		entryVersion, _, _ := unstructured.NestedString(values, "version")
		if entryVersion != version {
			continue
		}
		return renderedFeatureSet{
			name:     name,
			enabled:  featureNames(values, "enabled"),
			disabled: featureNames(values, "disabled"),
		}, nil
	}
	return renderedFeatureSet{}, fmt.Errorf("%s renders no entry for version %s", filepath.Base(path), version)
}

// featureNames returns the feature names under one status entry list.
func featureNames(entry map[string]any, list string) []string {
	items, _, _ := unstructured.NestedSlice(entry, list)
	names := make([]string, 0, len(items))
	for _, item := range items {
		values, _ := item.(map[string]any)
		name, _, _ := unstructured.NestedString(values, "name")
		names = append(names, name)
	}
	return names
}

// record writes the completed default feature set's entry for version
// onto the cluster's FeatureGate object, keeping the entries of the
// versions in the cluster's history.
func record(ctx context.Context, client dynamic.Interface, sets map[string]featureSet, version string) error {
	object, err := client.Resource(featureGates).Get(ctx, "cluster", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read the cluster FeatureGate: %w", err)
	}

	// The completed entry the config operator records: the default
	// set's enabled features, and every feature the release knows for
	// this version that the set does not enable, so each one is
	// explicitly on or explicitly off.
	deflt := sets[""]
	var known []string
	for _, set := range sets {
		known = append(known, set.enabled...)
		known = append(known, set.disabled...)
	}
	disabled := slices.Clone(deflt.disabled)
	for _, name := range known {
		if !slices.Contains(deflt.enabled, name) && !slices.Contains(disabled, name) {
			disabled = append(disabled, name)
		}
	}
	sort.Strings(disabled)
	entry := map[string]any{
		"version":  version,
		"enabled":  featureList(deflt.enabled),
		"disabled": featureList(disabled),
	}

	// The cluster's history decides which older entries stay: the
	// current version's entry is fresh, and the entry of a version the
	// cluster no longer records is obsolete.
	history := map[string]bool{version: true}
	clusterVersion, err := client.Resource(clusterVersions).Get(ctx, "version", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read the cluster version: %w", err)
	}
	if entries, found, _ := unstructured.NestedSlice(clusterVersion.Object, "status", "history"); found {
		for _, item := range entries {
			values, _ := item.(map[string]any)
			if entryVersion, _, _ := unstructured.NestedString(values, "version"); entryVersion != "" {
				history[entryVersion] = true
			}
		}
	}

	entries := []any{entry}
	if existing, found, _ := unstructured.NestedSlice(object.Object, "status", "featureGates"); found {
		for _, item := range existing {
			values, _ := item.(map[string]any)
			entryVersion, _, _ := unstructured.NestedString(values, "version")
			if history[entryVersion] && entryVersion != version {
				entries = append(entries, values)
			}
		}
	}
	if err := unstructured.SetNestedSlice(object.Object, entries, "status", "featureGates"); err != nil {
		return err
	}
	_, err = client.Resource(featureGates).UpdateStatus(ctx, object, metav1.UpdateOptions{})
	return err
}

// featureList shapes feature names the way the API stores them.
func featureList(names []string) []any {
	list := make([]any, len(names))
	for i, name := range names {
		list[i] = map[string]any{"name": name}
	}
	return list
}
