package supervisor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// syncBuffer collects output written concurrently by the copy goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// stopper returns a process that announces startup, runs until it receives
// SIGTERM, and then records its name in the order file. The signal handler
// is installed before startup is announced.
func stopper(name, orderFile string) Process {
	script := fmt.Sprintf(
		"trap 'echo %s >> %q; exit 0' TERM; echo started; while :; do sleep 0.1; done",
		name, orderFile)
	return Process{Name: name, Path: "bash", Args: []string{"-c", script}}
}

func waitForLine(t *testing.T, b *syncBuffer, line string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(b.String(), line) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in output:\n%s", line, b.String())
}

func waitForRun(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return in time")
	}
	return nil
}

func readOrder(t *testing.T, orderFile string) []string {
	t.Helper()
	data, err := os.ReadFile(orderFile)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var names []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	return names
}

func TestRunForwardsOutputWithComponentPrefix(t *testing.T) {
	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- (&Supervisor{Stdout: stdout, Stderr: stderr}).Run(context.Background(), []Process{
			{Name: "writer", Path: "bash", Args: []string{
				"-c", "echo out-one; echo err-one 1>&2; echo out-two; printf no-trailing-newline",
			}},
		})
	}()

	err := waitForRun(t, done)
	if err == nil || !strings.Contains(err.Error(), "writer") {
		t.Fatalf("expected error naming the exited child, got: %v", err)
	}
	waitForLine(t, stdout, "[writer] out-one")
	waitForLine(t, stdout, "[writer] no-trailing-newline")
	waitForLine(t, stderr, "[writer] err-one")
	if strings.Contains(stdout.String(), "err-one") {
		t.Errorf("child stderr leaked into supervisor stdout:\n%s", stdout.String())
	}
}

func TestRunCancellationTerminatesChildrenInReverseOrder(t *testing.T) {
	orderFile := filepath.Join(t.TempDir(), "order")
	out := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (&Supervisor{Stdout: out, Stderr: out}).Run(ctx, []Process{
			stopper("first", orderFile),
			stopper("second", orderFile),
			stopper("third", orderFile),
		})
	}()

	for _, name := range []string{"first", "second", "third"} {
		waitForLine(t, out, fmt.Sprintf("[%s] started", name))
	}
	cancel()

	if err := waitForRun(t, done); err != nil {
		t.Fatalf("Run returned an error after cancellation: %v", err)
	}
	want := []string{"third", "second", "first"}
	if got := readOrder(t, orderFile); !reflect.DeepEqual(got, want) {
		t.Fatalf("children were terminated in order %v, want %v", got, want)
	}
}

func TestRunSignalTerminatesChildrenInReverseOrder(t *testing.T) {
	orderFile := filepath.Join(t.TempDir(), "order")
	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- (&Supervisor{Stdout: out, Stderr: out}).Run(context.Background(), []Process{
			stopper("first", orderFile),
			stopper("second", orderFile),
		})
	}()

	waitForLine(t, out, "[second] started")
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}

	if err := waitForRun(t, done); err != nil {
		t.Fatalf("Run returned an error after SIGINT: %v", err)
	}
	want := []string{"second", "first"}
	if got := readOrder(t, orderFile); !reflect.DeepEqual(got, want) {
		t.Fatalf("children were terminated in order %v, want %v", got, want)
	}
}

func TestRunFailsFastWhenChildExits(t *testing.T) {
	dir := t.TempDir()
	orderFile := filepath.Join(dir, "order")
	readyFile := filepath.Join(dir, "ready")

	// The first child installs its signal handler before announcing
	// readiness, so the second child only exits once the first is able to
	// shut down cleanly.
	first := Process{
		Name: "first",
		Path: "bash",
		Args: []string{"-c", fmt.Sprintf(
			"trap 'echo first >> %q; exit 0' TERM; touch %q; while :; do sleep 0.1; done",
			orderFile, readyFile)},
	}
	second := Process{
		Name: "second",
		Path: "bash",
		Args: []string{"-c", fmt.Sprintf(
			"while [ ! -f %q ]; do sleep 0.05; done; exit 7", readyFile)},
	}

	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- (&Supervisor{Stdout: out, Stderr: out}).Run(context.Background(), []Process{first, second})
	}()

	err := waitForRun(t, done)
	if err == nil {
		t.Fatal("expected an error when a child exits")
	}
	if !strings.Contains(err.Error(), "second exited") || !strings.Contains(err.Error(), "exit status 7") {
		t.Fatalf("error does not identify the failed child: %v", err)
	}
	if got := readOrder(t, orderFile); !reflect.DeepEqual(got, []string{"first"}) {
		t.Fatalf("remaining children were not torn down, termination order: %v", got)
	}
}

func TestRunFailsFastWhenChildExitsCleanly(t *testing.T) {
	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- (&Supervisor{Stdout: out, Stderr: out}).Run(context.Background(), []Process{
			{Name: "sleeper", Path: "sleep", Args: []string{"0.2"}},
		})
	}()

	err := waitForRun(t, done)
	if err == nil || !strings.Contains(err.Error(), "sleeper exited unexpectedly") {
		t.Fatalf("expected error for a cleanly exited child, got: %v", err)
	}
}

func TestRunStartFailureDoesNotStartLaterChildren(t *testing.T) {
	orderFile := filepath.Join(t.TempDir(), "order")
	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- (&Supervisor{Stdout: out, Stderr: out}).Run(context.Background(), []Process{
			{Name: "missing", Path: filepath.Join(t.TempDir(), "missing-binary")},
			stopper("never", orderFile),
		})
	}()

	err := waitForRun(t, done)
	if err == nil || !strings.Contains(err.Error(), "start missing") {
		t.Fatalf("expected start failure error, got: %v", err)
	}
	if got := readOrder(t, orderFile); len(got) != 0 {
		t.Fatalf("later children should not have been started, termination order: %v", got)
	}
	if strings.Contains(out.String(), "[never]") {
		t.Fatalf("later child produced output:\n%s", out.String())
	}
}

func TestRunKillsChildThatIgnoresTermination(t *testing.T) {
	shutdownGrace = 200 * time.Millisecond
	t.Cleanup(func() { shutdownGrace = 5 * time.Second })

	out := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (&Supervisor{Stdout: out, Stderr: out}).Run(ctx, []Process{
			{Name: "stubborn", Path: "bash", Args: []string{
				"-c", "trap '' TERM; echo started; while :; do sleep 0.1; done",
			}},
		})
	}()

	waitForLine(t, out, "[stubborn] started")
	cancel()

	if err := waitForRun(t, done); err != nil {
		t.Fatalf("Run returned an error after cancellation: %v", err)
	}
}
