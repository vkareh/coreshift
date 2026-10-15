package image

import (
	"context"
	"errors"
	"os"
	"testing"
)

// The pinned release payload and the project test pull secret used for
// live verification against the real registry. The test skips when the
// secret is absent, so environments without it still pass.
const (
	smokePullSecretPath = "./pull-secret"
	smokeReleaseImage   = "quay.io/openshift-release-dev/ocp-release:4.22.8-x86_64"
)

// TestExtractKubeComponentsSmoke resolves the hyperkube image from the
// pinned release payload, pulls it from the real registry, and extracts
// the core kube binaries end to end.
func TestExtractKubeComponentsSmoke(t *testing.T) {
	pullSecret, err := os.ReadFile(smokePullSecretPath)
	if errors.Is(err, os.ErrNotExist) {
		t.Skipf("pull secret not found at %s", smokePullSecretPath)
	}
	if err != nil {
		t.Fatal(err)
	}

	client, err := NewClient(pullSecret)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	releaseFilesystem := t.TempDir()
	if err := client.PullAndUnpack(ctx, smokeReleaseImage, releaseFilesystem); err != nil {
		t.Fatal(err)
	}

	binaries, err := client.ExtractKubeComponents(ctx, releaseFilesystem, t.TempDir())
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
}
