package image

import (
	"os"
	"path/filepath"
	"testing"
)

const testReleaseManifest = `{
  "kind": "ImageStream",
  "apiVersion": "image.openshift.io/v1",
  "spec": {
    "tags": [
      {
        "name": "etcd",
        "from": {
          "kind": "DockerImage",
          "name": "quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:etcddigest"
        }
      },
      {
        "name": "hyperkube",
        "from": {
          "kind": "DockerImage",
          "name": "quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:hyperkubedigest"
        }
      }
    ]
  }
}`

func writeTestReleaseManifest(t *testing.T, root string) {
	t.Helper()
	path := filepath.Join(root, releaseManifestPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(testReleaseManifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveComponentImage(t *testing.T) {
	root := t.TempDir()
	writeTestReleaseManifest(t, root)

	got, err := ResolveComponentImage(root, "etcd")
	if err != nil {
		t.Fatal(err)
	}
	if want := "quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:etcddigest"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestResolveComponentImageNotFound(t *testing.T) {
	root := t.TempDir()
	writeTestReleaseManifest(t, root)

	if _, err := ResolveComponentImage(root, "does-not-exist"); err == nil {
		t.Fatal("expected error for missing component")
	}
}

func TestResolveComponentImageMissingManifest(t *testing.T) {
	if _, err := ResolveComponentImage(t.TempDir(), "etcd"); err == nil {
		t.Fatal("expected error for missing manifest")
	}
}
