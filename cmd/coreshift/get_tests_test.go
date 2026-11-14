package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cacheEntry mirrors the cache layout for an image reference: the
// reference's colons become dashes and its slashes stay directories.
func cacheEntry(t *testing.T, kind, imageRef string) string {
	t.Helper()
	base := os.Getenv("XDG_CACHE_HOME")
	return filepath.Join(base, "coreshift", kind, filepath.FromSlash(strings.ReplaceAll(imageRef, ":", "-")))
}

// seedTestsCache pre-populates everything runGetTests resolves without a
// registry: the tag record for the recorded release, the cached release
// manifest naming the tests image, and the cached tests binary.
func seedTestsCache(t *testing.T, releaseTag, releaseDigest, testsImage string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(cacheEntry(t, "tags", releaseTag)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cacheEntry(t, "tags", releaseTag), []byte(releaseDigest), 0o644); err != nil {
		t.Fatal(err)
	}
	manifestDir := filepath.Join(cacheEntry(t, "release", releaseDigest), "release-manifests")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"kind":"ImageStream","apiVersion":"image.openshift.io/v1","spec":{"tags":[
		{"name":"tests","from":{"kind":"DockerImage","name":"` + testsImage + `"}}]}}`
	if err := os.WriteFile(filepath.Join(manifestDir, "image-references"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	binaryDir := cacheEntry(t, "components", testsImage)
	if err := os.MkdirAll(binaryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binaryDir, "openshift-tests"), []byte("suite"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestRunGetTestsPrintsCachedBinary checks the whole resolution with a
// warm cache: the recorded release resolves through its tag record, the
// cached release manifest names the tests image, and the cached binary's
// path is printed — no registry, no cluster running.
func TestRunGetTestsPrintsCachedBinary(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	stateDir := filepath.Join(state, "coreshift", "clusters", "dev")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeMarker(t, stateDir, "dev", "reg.example/ocp-release:4.22.8-x86_64")
	seedTestsCache(t, "reg.example/ocp-release:4.22.8-x86_64",
		"reg.example/ocp-release@sha256:reldigest",
		"quay.io/example/tests@sha256:testsdigest")
	secret := filepath.Join(t.TempDir(), "pull-secret")
	if err := os.WriteFile(secret, []byte(`{"auths":{"reg.example":{"auth":"eHo6eHo="}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var outErr error
	out := captureStdout(t, func() { outErr = runGetTests(t.Context(), "dev", secret) })
	if outErr != nil {
		t.Fatal(outErr)
	}
	want := cacheEntry(t, "components", "quay.io/example/tests@sha256:testsdigest") + "/openshift-tests"
	if got := strings.TrimSpace(out); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

// TestRunGetTestsMissingCluster checks that no recorded release is a
// plain refusal, like the kubeconfig getter's.
func TestRunGetTestsMissingCluster(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	writeStateTree(t, filepath.Join(base, "coreshift", "clusters", "alpha"), "alpha")

	err := runGetTests(t.Context(), "nope", "unused")
	if err == nil || !strings.Contains(err.Error(), "no cluster named nope") {
		t.Fatalf("err = %v, want a plain refusal", err)
	}
}

// TestGetTestsFlagDefaults checks that the tests getter requires the
// pull secret and names the cluster like its siblings.
func TestGetTestsFlagDefaults(t *testing.T) {
	cmd := newGetTestsCommand()
	if flag := cmd.Flags().Lookup("pull-secret"); flag == nil {
		t.Fatal("flag --pull-secret is not defined")
	} else if err := cmd.MarkFlagRequired("pull-secret"); err != nil {
		t.Fatalf("flag --pull-secret is not required: %v", err)
	}
	if flag := cmd.Flags().Lookup("name"); flag == nil || flag.DefValue != "coreshift" {
		t.Fatalf("--name default = %v, want coreshift", flag)
	}
}
