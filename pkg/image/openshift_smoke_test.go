package image

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestExtractOpenShiftComponentsSmoke resolves each OpenShift API layer
// component's image from the cached release manifest, confirms the five
// components are five distinct images, pulls each from the real
// registry, extracts the binaries, and executes them on the host.
func TestExtractOpenShiftComponentsSmoke(t *testing.T) {
	pullSecret, err := os.ReadFile(smokePullSecretPath)
	if errors.Is(err, os.ErrNotExist) {
		t.Skipf("pull secret not found at %s", smokePullSecretPath)
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

	// Each component must resolve to its own image, since unlike hyperkube
	// they are built separately.
	specs := make(map[string]string, len(openshiftComponents))
	for _, component := range openshiftComponents {
		spec, err := ResolveComponentImage(releaseRoot, component.component)
		if err != nil {
			t.Fatalf("resolve %s: %v", component.component, err)
		}
		for other, seen := range specs {
			if seen == spec {
				t.Fatalf("%s and %s resolve to the same image %s", other, component.component, spec)
			}
		}
		specs[component.component] = spec
	}

	binaries, err := client.ExtractOpenShiftComponents(ctx, releaseRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(binaries) != len(openshiftComponents) {
		t.Fatalf("got %d binaries, want %d", len(binaries), len(openshiftComponents))
	}
	for component, path := range binaries {
		// openshift-controller-manager links beyond glibc; its bundled
		// shared libraries are extracted next to the binary, so the
		// loader is pointed there.
		cmd := exec.Command(path, "--help")
		cmd.Env = append(os.Environ(), "LD_LIBRARY_PATH="+filepath.Dir(path))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s --help: %v\n%s", component, err, out)
		}
	}
}
