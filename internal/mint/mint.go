// Package mint builds the container images CoreShift runs its podman
// mode from: a copy of the running binary and a build recipe, built with
// podman and tagged so an unchanged binary skips the build. The control
// plane's image runs on a minimal Red Hat base; the worker's runs on the
// payload's rhel-coreos image, which carries the node binaries the
// release ships.
package mint

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/vkareh/coreshift/pkg/image"
)

//go:embed Containerfile
var containerfile string

// The repositories the two images build into, below localhost so nothing
// is published to any registry.
const (
	controlPlaneRepository = "coreshift"
	workerRepository       = "coreshift-worker"
)

// workerContainerfile is the worker image's build recipe. The base is the
// payload's rhel-coreos image, digest-pinned from the release's component
// map, so the node binaries in it match the release the control plane
// runs. The binary goes under the image's writable var tree, because the
// ostree layout symlinks /usr/local into it and owns the rest of /usr.
// The stop signal is the one the supervisor handles; the base image
// declares systemd's, which nothing in the container answers.
const workerContainerfile = `FROM %s
COPY coreshift /var/usrlocal/bin/coreshift
STOPSIGNAL SIGTERM
ENTRYPOINT ["/var/usrlocal/bin/coreshift", "run", "worker"]
`

// logTailLines is how many lines of the captured build output a failure
// error includes, matching the supervisor's per-child tail.
const logTailLines = 20

// buildLogName is the file, under the image cache, the captured output
// of a failed build is kept in.
const buildLogName = "build.log"

// Image returns the reference of the control plane's container image,
// built from the running binary when no image carries its tag yet. The
// tag combines the binary's version with a short digest of its content,
// so a rebuilt or dirty binary whose version repeats still gets its own
// tag, and an unchanged binary maps to the tag already built. Building
// shells out to podman; docker is not supported. The build is quiet like
// the control plane: its output is captured and shown only when it
// fails, unless verbose streams it live.
func Image(ctx context.Context, version string, verbose bool) (string, error) {
	return build(ctx, controlPlaneRepository, version, containerfile, "", verbose)
}

// WorkerImage returns the reference of the worker's container image,
// built the same way from the running binary but on the payload's
// rhel-coreos image, whose pullspec the caller resolves from the release
// the target cluster runs. The build pulls that base from its registry,
// so it takes the pull secret as podman's authfile.
func WorkerImage(ctx context.Context, version, base, authfile string, verbose bool) (string, error) {
	return build(ctx, workerRepository, version, fmt.Sprintf(workerContainerfile, base), authfile, verbose)
}

// build returns the reference of the image built from the running binary
// and the given recipe, building it with podman when no image carries
// its tag yet. The tag combines the binary's version with a short digest
// of the build's inputs, so a rebuilt binary or a different recipe still
// gets its own tag, and an unchanged build maps to the tag already
// built. The build is quiet: its output is captured and shown only when
// it fails, unless verbose streams it live.
func build(ctx context.Context, repo, version, file, authfile string, verbose bool) (string, error) {
	if _, err := exec.LookPath("podman"); err != nil {
		return "", fmt.Errorf("podman is required to build the container image: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate running binary: %w", err)
	}
	sum, err := contextDigest(exe, file)
	if err != nil {
		return "", err
	}
	ref := "localhost/" + repo + ":" + stamp(version, sum)

	if cmd := exec.CommandContext(ctx, "podman", "image", "exists", ref); cmd.Run() == nil {
		return ref, nil
	}
	dir, err := os.MkdirTemp("", "coreshift-build-")
	if err != nil {
		return "", fmt.Errorf("create build context: %w", err)
	}
	defer os.RemoveAll(dir)
	if err := writeContext(dir, exe, file); err != nil {
		return "", err
	}
	fmt.Println("Building container image", ref)
	// Both streams go to one writer; the command shares a single pipe
	// for identical writers, so the capture is merged and race-free.
	var out bytes.Buffer
	w := io.Writer(&out)
	if verbose {
		w = io.MultiWriter(os.Stderr, &out)
	}
	args := []string{"build"}
	if authfile != "" {
		args = append(args, "--authfile", authfile)
	}
	args = append(args, "-t", ref, dir)
	build := exec.CommandContext(ctx, "podman", args...)
	build.Stdout = w
	build.Stderr = w
	if err := build.Run(); err != nil {
		return "", buildFailure(err, out.String())
	}
	return ref, nil
}

// buildFailure reports a failed image build: the error gains the recent
// tail of the captured build output and the full log's path, like the
// control plane's own child failures.
func buildFailure(err error, output string) error {
	err = fmt.Errorf("build container image: %w", err)
	path, logErr := keepBuildLog(output)
	tail := tailLines(output, logTailLines)
	if logErr != nil || tail == "" {
		return err
	}
	return fmt.Errorf("%w\n\nrecent log output:\n%s\n\nfull log: %s", err, tail, path)
}

// keepBuildLog writes the captured output of a failed build to its file
// under the image cache and returns the file's path.
func keepBuildLog(output string) (string, error) {
	dir, err := image.CacheRoot()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, buildLogName)
	return path, os.WriteFile(path, []byte(output), 0o644)
}

// tailLines returns the last lines of text.
func tailLines(text string, lines int) string {
	all := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n")
}

// stamp returns the image tag for a binary: its version plus the first
// bytes of its content digest.
func stamp(version, digest string) string {
	return version + "-" + digest[:12]
}

// writeContext fills dir with the podman build context: a copy of the
// binary at exe and the given Containerfile.
func writeContext(dir, exe, file string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create build context: %w", err)
	}
	src, err := os.Open(exe)
	if err != nil {
		return fmt.Errorf("read running binary: %w", err)
	}
	defer src.Close()
	dst, err := os.OpenFile(filepath.Join(dir, "coreshift"), os.O_CREATE|os.O_WRONLY, 0o755)
	if err != nil {
		return fmt.Errorf("create context binary: %w", err)
	}
	_, copyErr := io.Copy(dst, src)
	closeErr := dst.Close()
	if copyErr != nil {
		return fmt.Errorf("copy running binary: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("copy running binary: %w", closeErr)
	}
	if err := os.WriteFile(filepath.Join(dir, "Containerfile"), []byte(file), 0o644); err != nil {
		return fmt.Errorf("write Containerfile: %w", err)
	}
	return nil
}

// contextDigest returns the SHA-256 of the build's inputs, the binary
// and the recipe, as hex. The recipe names the base image, so a new
// release's base gets its own tag too.
func contextDigest(exe, file string) (string, error) {
	binary, err := os.ReadFile(exe)
	if err != nil {
		return "", fmt.Errorf("read running binary: %w", err)
	}
	sum := sha256.New()
	sum.Write(binary)
	sum.Write([]byte(file))
	return fmt.Sprintf("%x", sum.Sum(nil)), nil
}
