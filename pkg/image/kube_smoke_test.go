package image

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
)

// The pinned release payload and the project test pull secret used for
// live verification against the real registry. The test skips when the
// secret is absent, so environments without it still pass.
const (
	smokeReleaseImage = "quay.io/openshift-release-dev/ocp-release:4.22.8-x86_64"

	// Developer default; CI overrides it with SMOKE_PULL_SECRET_PATH.
	smokePullSecretDefault = "./pull-secret"
)

func smokePullSecretPath() string {
	if p := os.Getenv("SMOKE_PULL_SECRET_PATH"); p != "" {
		return p
	}
	return smokePullSecretDefault
}

func TestExtractKubeComponentsSmoke(t *testing.T) {
	pullSecret, err := os.ReadFile(smokePullSecretPath())
	if errors.Is(err, os.ErrNotExist) {
		t.Skipf("pull secret not found at %s", smokePullSecretPath())
	}
	if err != nil {
		t.Fatal(err)
	}
	// Keep the smoke test's cache separate from the user's.
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	client, err := NewClient(pullSecret)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	releaseRoot, err := client.CachedRelease(ctx, smokeReleaseImage)
	if err != nil {
		t.Fatal(err)
	}

	binaries, err := client.ExtractKubeComponents(ctx, releaseRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(binaries) != len(kubeComponents) {
		t.Fatalf("got %d binaries, want %d", len(binaries), len(kubeComponents))
	}
	for component, path := range binaries {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("%s: %v", component, err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("%s is not executable", component)
		}
		if info.Size() == 0 {
			t.Fatalf("%s is empty", component)
		}
	}

	// A second call must return the same paths straight from the cache.
	again, err := client.ExtractKubeComponents(ctx, releaseRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(binaries, again) {
		t.Fatalf("second call returned %v, want %v", again, binaries)
	}
}
