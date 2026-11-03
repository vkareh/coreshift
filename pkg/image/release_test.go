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

// writeTestPayload writes the release image's manifest directories: the
// operator's own manifests and the release payload's, each with one
// file standing in for the set.
func writeTestPayload(t *testing.T, root string) {
	t.Helper()
	operatorManifest := filepath.Join(root, "manifests", "deployment.yaml")
	if err := os.MkdirAll(filepath.Dir(operatorManifest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(operatorManifest, []byte("kind: Deployment\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTestReleaseManifest(t, root)
	metadata := filepath.Join(root, releaseManifestDir, "release-metadata")
	if err := os.WriteFile(metadata, []byte(`{"kind":"cincinnati-metadata-v0","version":"4.22.21"}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPayloadVersion(t *testing.T) {
	payload := t.TempDir()
	writeTestPayload(t, payload)

	version, err := PayloadVersion(payload)
	if err != nil {
		t.Fatal(err)
	}
	if version != "4.22.21" {
		t.Fatalf("version = %q, want 4.22.21", version)
	}
}

func TestPayloadVersionMissingMetadata(t *testing.T) {
	if _, err := PayloadVersion(t.TempDir()); err == nil {
		t.Fatal("expected error for missing release metadata")
	}
}

func TestExtractPayload(t *testing.T) {
	filesystem := t.TempDir()
	writeTestPayload(t, filesystem)
	dir := t.TempDir()

	if err := extractPayload(filesystem, dir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		filepath.Join("manifests", "deployment.yaml"),
		filepath.Join(releaseManifestDir, "image-references"),
		filepath.Join(releaseManifestDir, "release-metadata"),
	} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s was not extracted: %v", name, err)
		}
	}
}

// TestCachedPayloadUsesRecordedDigest covers the second run of a pinned
// release: with the tag's digest recorded and the payload cached, no
// registry credentials are needed at all, so the client is built with
// none and any registry call would fail the test.
func TestCachedPayloadUsesRecordedDigest(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	release := "quay.io/example/release:v1"
	if err := writeTagRecord(release, testDigestRef); err != nil {
		t.Fatal(err)
	}
	dir, err := cachePath(payloadKind, testDigestRef)
	if err != nil {
		t.Fatal(err)
	}
	writeTestPayload(t, dir)

	root, err := (&Client{}).CachedPayload(context.Background(), release)
	if err != nil {
		t.Fatal(err)
	}
	if root != dir {
		t.Fatalf("root = %q, want %q", root, dir)
	}
	if _, err := ResolveComponentImage(root, "etcd"); err != nil {
		t.Fatalf("resolve from cached payload: %v", err)
	}
}
