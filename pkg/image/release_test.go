package image

import (
	"context"
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

func TestTagRecordRoundTrip(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	release := "quay.io/example/release:v1"
	if recorded, ok := readTagRecord(release); ok {
		t.Fatalf("recorded %q on a fresh cache, want none", recorded)
	}
	if err := writeTagRecord(release, testDigestRef); err != nil {
		t.Fatal(err)
	}
	if recorded, ok := readTagRecord(release); !ok || recorded != testDigestRef {
		t.Fatalf("recorded %q, %v; want %q", recorded, ok, testDigestRef)
	}
}

func TestExtractReleaseManifest(t *testing.T) {
	filesystem := t.TempDir()
	writeTestReleaseManifest(t, filesystem)
	dir := t.TempDir()

	if err := extractReleaseManifest(filesystem, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveComponentImage(dir, "etcd"); err != nil {
		t.Fatalf("resolve from extracted manifest: %v", err)
	}
}

// TestCachedReleaseUsesRecordedDigest covers the second run of a pinned
// release: with the tag's digest recorded and the manifest cached, no
// registry credentials are needed at all, so the client is built with
// none and any registry call would fail the test.
func TestCachedReleaseUsesRecordedDigest(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	release := "quay.io/example/release:v1"
	if err := writeTagRecord(release, testDigestRef); err != nil {
		t.Fatal(err)
	}
	dir, err := cachePath(releaseKind, testDigestRef)
	if err != nil {
		t.Fatal(err)
	}
	writeTestReleaseManifest(t, dir)

	root, err := (&Client{}).CachedRelease(context.Background(), release)
	if err != nil {
		t.Fatal(err)
	}
	if root != dir {
		t.Fatalf("root = %q, want %q", root, dir)
	}
	if _, err := ResolveComponentImage(root, "etcd"); err != nil {
		t.Fatalf("resolve from cached manifest: %v", err)
	}
}
