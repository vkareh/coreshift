package mint

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStampCombinesVersionAndDigest(t *testing.T) {
	const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	// A dirty version string and a rebuilt binary share the version; the
	// digest keeps their tags apart.
	if got, want := stamp("v0.1.0-dirty", digest), "v0.1.0-dirty-0123456789ab"; got != want {
		t.Fatalf("stamp = %q, want %q", got, want)
	}
}

func TestBuildContextWritesBinaryAndContainerfile(t *testing.T) {
	base := t.TempDir()
	contextDir := filepath.Join(base, "context")
	binary := filepath.Join(base, "coreshift")
	if err := os.WriteFile(binary, []byte("fake binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := writeContext(contextDir, binary); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(contextDir, "coreshift")); err != nil || string(got) != "fake binary" {
		t.Fatalf("context binary = %q, %v", got, err)
	}
	info, err := os.Stat(filepath.Join(contextDir, "coreshift"))
	if err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("context binary is not executable: %v", err)
	}
	file, err := os.ReadFile(filepath.Join(contextDir, "Containerfile"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"FROM gcr.io/distroless/cc-debian12@sha256:",
		"COPY coreshift /usr/local/bin/coreshift",
		"ENV CORESHIFT_CONTAINER=1",
		`ENTRYPOINT ["/usr/local/bin/coreshift", "run", "cluster"]`,
	} {
		if !strings.Contains(string(file), want) {
			t.Fatalf("Containerfile missing %q:\n%s", want, file)
		}
	}
}

// writeFakePodman writes a podman stand-in into a directory of its own
// and prepends that directory to PATH. It records every call, one line
// per call, in the file named by PODMAN_LOG and answers the image-exists
// check with existsStatus.
func writeFakePodman(t *testing.T, existsStatus int) {
	t.Helper()
	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> "$PODMAN_LOG"
if [ "$1" = "image" ]; then
	exit %d
fi
exit 0
`, existsStatus)
	path := filepath.Join(dir, "podman")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("PODMAN_LOG", filepath.Join(t.TempDir(), "calls"))
}

// podmanCalls returns the calls the fake podman recorded.
func podmanCalls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(os.Getenv("PODMAN_LOG"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// wantedRef returns the reference Image should report for version: the
// test binary's own tag.
func wantedRef(t *testing.T, version string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	sum, err := fileDigest(exe)
	if err != nil {
		t.Fatal(err)
	}
	return "localhost/coreshift:" + stamp(version, sum)
}

func TestImageSkipsBuildWhenTagExists(t *testing.T) {
	writeFakePodman(t, 0)

	ref, err := Image(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if want := wantedRef(t, "test"); ref != want {
		t.Fatalf("ref = %q, want %q", ref, want)
	}
	if calls := podmanCalls(t); len(calls) != 1 || calls[0] != "image exists "+ref {
		t.Fatalf("podman calls = %q, want only the exists check for %s", calls, ref)
	}
}

func TestImageBuildsWhenTagMissing(t *testing.T) {
	writeFakePodman(t, 1)

	ref, err := Image(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if want := wantedRef(t, "test"); ref != want {
		t.Fatalf("ref = %q, want %q", ref, want)
	}
	calls := podmanCalls(t)
	if len(calls) != 2 {
		t.Fatalf("podman calls = %q, want the exists check and the build", calls)
	}
	if calls[0] != "image exists "+ref {
		t.Fatalf("first podman call = %q, want the exists check for %s", calls[0], ref)
	}
	if !strings.HasPrefix(calls[1], "build -t "+ref+" ") {
		t.Fatalf("second podman call = %q, want a build of %s", calls[1], ref)
	}
}

func TestImageRequiresPodman(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	_, err := Image(context.Background(), "test")
	if err == nil || !strings.Contains(err.Error(), "podman") {
		t.Fatalf("err = %v, want an error naming podman", err)
	}
}
