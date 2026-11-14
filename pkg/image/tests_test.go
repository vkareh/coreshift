package image

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractTestsUsesCachedImage(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	root := t.TempDir()
	writeTestReleaseManifest(t, root)

	dir, err := cachePath(componentsKind, "quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:testsdigest")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "openshift-tests"), []byte("suite"), 0o755); err != nil {
		t.Fatal(err)
	}

	path, err := (&Client{}).ExtractTests(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "openshift-tests"); path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
}

func TestExtractTestsMissingComponent(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	root := t.TempDir()
	path := filepath.Join(root, releaseManifestPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"kind":"ImageStream","apiVersion":"image.openshift.io/v1","spec":{"tags":[
		{"name":"etcd","from":{"kind":"DockerImage","name":"quay.io/example/etcd@sha256:etcddigest"}}]}}`
	if err := os.WriteFile(path, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := (&Client{}).ExtractTests(context.Background(), root); err == nil || !strings.Contains(err.Error(), "tests") {
		t.Fatalf("err = %v, want missing tests component", err)
	}
}
