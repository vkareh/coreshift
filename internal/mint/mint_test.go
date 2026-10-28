package mint

import (
	"context"
	"fmt"
	"io"
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

	if err := writeContext(contextDir, binary, containerfile); err != nil {
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
		"FROM registry.access.redhat.com/ubi10/ubi-minimal@sha256:",
		"FROM registry.access.redhat.com/ubi10/ubi-micro@sha256:",
		"COPY --from=certificates /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
		"ENV SSL_CERT_FILE=",
		"COPY coreshift /usr/local/bin/coreshift",
		"ENV CORESHIFT_CONTAINER=1",
		`ENTRYPOINT ["/usr/local/bin/coreshift", "run", "cluster"]`,
	} {
		if !strings.Contains(string(file), want) {
			t.Fatalf("Containerfile missing %q:\n%s", want, file)
		}
	}
}

// TestWorkerContainerfile pins the worker image's recipe: the payload's
// rhel-coreos base, the binary under the image's writable var tree, the
// stop signal the supervisor handles, and the worker entrypoint.
func TestWorkerContainerfile(t *testing.T) {
	file := fmt.Sprintf(workerContainerfile, "reg.example/coreos@sha256:abc123")
	for _, want := range []string{
		"FROM reg.example/coreos@sha256:abc123",
		"COPY coreshift /var/usrlocal/bin/coreshift",
		"STOPSIGNAL SIGTERM",
		`ENTRYPOINT ["/var/usrlocal/bin/coreshift", "run", "worker"]`,
	} {
		if !strings.Contains(file, want) {
			t.Fatalf("worker Containerfile missing %q:\n%s", want, file)
		}
	}
}

// writeFakePodman writes a podman stand-in into a directory of its own
// and prepends that directory to PATH. It records every call, one line
// per call, in the file named by PODMAN_LOG, answers the image-exists
// check with existsStatus, and answers a build with two lines of output
// and the exit status named by PODMAN_BUILD_STATUS, zero when unset.
func writeFakePodman(t *testing.T, existsStatus int) {
	t.Helper()
	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> "$PODMAN_LOG"
if [ "$1" = "image" ]; then
	exit %d
fi
if [ "$1" = "build" ]; then
	echo "step 1/2: FROM scratch"
	echo "step 2/2: COPY coreshift /usr/local/bin/coreshift" >&2
	exit "${PODMAN_BUILD_STATUS:-0}"
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
	sum, err := contextDigest(exe, containerfile)
	if err != nil {
		t.Fatal(err)
	}
	return "localhost/coreshift:" + stamp(version, sum)
}

// wantedWorkerRef returns the reference WorkerImage should report for
// version and base: the test binary's own worker tag.
func wantedWorkerRef(t *testing.T, version, base string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	sum, err := contextDigest(exe, fmt.Sprintf(workerContainerfile, base))
	if err != nil {
		t.Fatal(err)
	}
	return "localhost/coreshift-worker:" + stamp(version, sum)
}

func TestImageSkipsBuildWhenTagExists(t *testing.T) {
	writeFakePodman(t, 0)

	ref, err := Image(context.Background(), "test", false)
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

	ref, err := Image(context.Background(), "test", false)
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

// TestImageBuildQuietOnSuccess checks that a successful build prints
// only the progress line: the build tool's output is captured, not
// shown.
func TestImageBuildQuietOnSuccess(t *testing.T) {
	writeFakePodman(t, 1)

	var err error
	stdout, stderr := captureStd(t, func() { _, err = Image(context.Background(), "test", false) })
	if err != nil {
		t.Fatal(err)
	}
	if want := "Building container image " + wantedRef(t, "test") + "\n"; stdout != want {
		t.Fatalf("stdout = %q, want only the progress line %q", stdout, want)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want nothing from the build tool", stderr)
	}
}

// TestImageBuildVerboseStreamsLive checks that a verbose build forwards
// the build tool's output live.
func TestImageBuildVerboseStreamsLive(t *testing.T) {
	writeFakePodman(t, 1)

	var err error
	_, stderr := captureStd(t, func() { _, err = Image(context.Background(), "test", true) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"step 1/2", "step 2/2"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("verbose stderr missing %q:\n%s", want, stderr)
		}
	}
}

// TestImageBuildFailureShowsTailAndLog checks that a failed build
// reports the recent tail of its captured output and where the full log
// lives, keeping the log there.
func TestImageBuildFailureShowsTailAndLog(t *testing.T) {
	writeFakePodman(t, 1)
	t.Setenv("PODMAN_BUILD_STATUS", "1")
	cacheBase := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheBase)

	_, err := Image(context.Background(), "test", false)
	if err == nil {
		t.Fatal("expected the build to fail")
	}
	log := filepath.Join(cacheBase, "coreshift", "build.log")
	for _, want := range []string{"recent log output:", "step 2/2", "full log: " + log} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q:\n%v", want, err)
		}
	}
	kept, readErr := os.ReadFile(log)
	if readErr != nil || !strings.Contains(string(kept), "step 1/2") {
		t.Fatalf("kept log = %q, %v, want the full build output", kept, readErr)
	}
}

func TestImageRequiresPodman(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	_, err := Image(context.Background(), "test", false)
	if err == nil || !strings.Contains(err.Error(), "podman") {
		t.Fatalf("err = %v, want an error naming podman", err)
	}
}

// TestWorkerImageBuildsWithAuthfile checks that the worker image builds
// under its own repository, tagged from the binary and the recipe naming
// the base, and that the build pulls the base with the pull secret as
// podman's authfile.
func TestWorkerImageBuildsWithAuthfile(t *testing.T) {
	writeFakePodman(t, 1)
	const base = "reg.example/coreos@sha256:abc123"

	ref, err := WorkerImage(context.Background(), "test", base, "/secret/pull-secret", false)
	if err != nil {
		t.Fatal(err)
	}
	if want := wantedWorkerRef(t, "test", base); ref != want {
		t.Fatalf("ref = %q, want %q", ref, want)
	}
	calls := podmanCalls(t)
	if len(calls) != 2 {
		t.Fatalf("podman calls = %q, want the exists check and the build", calls)
	}
	if calls[0] != "image exists "+ref {
		t.Fatalf("first podman call = %q, want the exists check for %s", calls[0], ref)
	}
	if !strings.HasPrefix(calls[1], "build --authfile /secret/pull-secret -t "+ref+" ") {
		t.Fatalf("second podman call = %q, want an authfile build of %s", calls[1], ref)
	}
}

// captureStd returns everything printed to standard output and standard
// error while run runs.
func captureStd(t *testing.T, run func()) (string, string) {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	defer func() {
		os.Stdout, os.Stderr = oldOut, oldErr
	}()
	stdout := make(chan string, 1)
	stderr := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(outR)
		outR.Close()
		stdout <- string(data)
	}()
	go func() {
		data, _ := io.ReadAll(errR)
		errR.Close()
		stderr <- string(data)
	}()
	run()
	outW.Close()
	errW.Close()
	return <-stdout, <-stderr
}
