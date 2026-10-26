// Package mint builds the container image CoreShift runs its podman
// mode from: a copy of the running binary and an embedded Containerfile,
// built with podman and tagged so an unchanged binary skips the build.
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

// logTailLines is how many lines of the captured build output a failure
// error includes, matching the supervisor's per-child tail.
const logTailLines = 20

// buildLogName is the file, under the image cache, the captured output
// of a failed build is kept in.
const buildLogName = "build.log"

// Image returns the reference of the container image built from the
// running binary, building it with podman when no image carries its tag
// yet. The tag combines the binary's version with a short digest of its
// content, so a rebuilt or dirty binary whose version repeats still gets
// its own tag, and an unchanged binary maps to the tag already built.
// Building shells out to podman; docker is not supported. The build is
// quiet like the control plane: its output is captured and shown only
// when it fails, unless verbose streams it live.
func Image(ctx context.Context, version string, verbose bool) (string, error) {
	if _, err := exec.LookPath("podman"); err != nil {
		return "", fmt.Errorf("podman is required to build the container image: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate running binary: %w", err)
	}
	sum, err := fileDigest(exe)
	if err != nil {
		return "", err
	}
	ref := "localhost/coreshift:" + stamp(version, sum)

	if cmd := exec.CommandContext(ctx, "podman", "image", "exists", ref); cmd.Run() == nil {
		return ref, nil
	}
	dir, err := os.MkdirTemp("", "coreshift-build-")
	if err != nil {
		return "", fmt.Errorf("create build context: %w", err)
	}
	defer os.RemoveAll(dir)
	if err := writeContext(dir, exe); err != nil {
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
	build := exec.CommandContext(ctx, "podman", "build", "-t", ref, dir)
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
// binary at exe and the embedded Containerfile.
func writeContext(dir, exe string) error {
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
	if err := os.WriteFile(filepath.Join(dir, "Containerfile"), []byte(containerfile), 0o644); err != nil {
		return fmt.Errorf("write Containerfile: %w", err)
	}
	return nil
}

// fileDigest returns the SHA-256 of a file's content as hex.
func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read running binary: %w", err)
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", fmt.Errorf("hash running binary: %w", err)
	}
	return fmt.Sprintf("%x", sum.Sum(nil)), nil
}
