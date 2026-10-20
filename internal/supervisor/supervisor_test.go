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

func readLogFile(t *testing.T, logDir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(logDir, name+".log"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// assertInOrder fails unless the lines appear in the given order in output.
func assertInOrder(t *testing.T, output string, lines ...string) {
	t.Helper()
	at := 0
	for _, line := range lines {
		i := strings.Index(output[at:], line)
		if i < 0 {
			t.Fatalf("expected %q in order in:\n%s", line, output)
		}
		at += i + len(line)
	}
}

func TestRunVerboseForwardsOutputWithComponentPrefix(t *testing.T) {
	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	logDir := t.TempDir()
	done := make(chan error, 1)
	go func() {
		done <- (&Supervisor{LogDir: logDir, Verbose: true, Stdout: stdout, Stderr: stderr, Progress: stdout}).Run(context.Background(), []Process{
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
	// Verbose runs keep writing the log files.
	log := readLogFile(t, logDir, "writer")
	for _, line := range []string{"out-one", "err-one", "out-two", "no-trailing-newline"} {
		if !strings.Contains(log, line) {
			t.Errorf("log file missing %q:\n%s", line, log)
		}
	}
	if strings.Contains(log, "[writer]") {
		t.Errorf("log file carries the live prefix:\n%s", log)
	}
}

func TestRunCapturesOutputToPerChildLogFiles(t *testing.T) {
	logDir := t.TempDir()
	progress := &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- (&Supervisor{LogDir: logDir, Progress: progress}).Run(context.Background(), []Process{
			{Name: "writer", Path: "bash", Args: []string{
				"-c", "echo out-line; echo err-line 1>&2; exit 5",
			}},
		})
	}()

	if err := waitForRun(t, done); err == nil {
		t.Fatal("expected an error when a child exits")
	}
	log := readLogFile(t, logDir, "writer")
	for _, line := range []string{"out-line", "err-line"} {
		if !strings.Contains(log, line) {
			t.Errorf("log file missing %q:\n%s", line, log)
		}
	}
	for _, leaked := range []string{"[writer]", "out-line", "err-line"} {
		if strings.Contains(progress.String(), leaked) {
			t.Errorf("quiet run leaked child output into progress lines:\n%s", progress.String())
		}
	}
}

func TestRunFailureIncludesRecentLogTail(t *testing.T) {
	logDir := t.TempDir()
	done := make(chan error, 1)
	go func() {
		done <- (&Supervisor{LogDir: logDir, Progress: &syncBuffer{}}).Run(context.Background(), []Process{
			{Name: "chatty", Path: "bash", Args: []string{
				"-c", "for i in $(seq 1 25); do echo line-$i; done; exit 9",
			}},
		})
	}()

	err := waitForRun(t, done)
	if err == nil {
		t.Fatal("expected an error when a child exits")
	}
	if !strings.Contains(err.Error(), "full log: "+filepath.Join(logDir, "chatty.log")) {
		t.Fatalf("error does not name the child's log file: %v", err)
	}
	if !strings.Contains(err.Error(), "line-25") {
		t.Fatalf("error missing the most recent log line: %v", err)
	}
	// Only the last logTailLines lines are included.
	if strings.Contains(err.Error(), "line-5\n") {
		t.Fatalf("error includes log lines older than the tail: %v", err)
	}
}

func TestRunCancellationTerminatesChildrenInReverseOrder(t *testing.T) {
	orderFile := filepath.Join(t.TempDir(), "order")
	progress, out := &syncBuffer{}, &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (&Supervisor{LogDir: t.TempDir(), Verbose: true, Stdout: out, Stderr: out, Progress: progress}).Run(ctx, []Process{
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
	assertInOrder(t, progress.String(), "stopping third", "stopping second", "stopping first")
}

func TestRunSignalTerminatesChildrenInReverseOrder(t *testing.T) {
	orderFile := filepath.Join(t.TempDir(), "order")
	progress, out := &syncBuffer{}, &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- (&Supervisor{LogDir: t.TempDir(), Verbose: true, Stdout: out, Stderr: out, Progress: progress}).Run(context.Background(), []Process{
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
	assertInOrder(t, progress.String(), "stopping second", "stopping first")
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

	done := make(chan error, 1)
	go func() {
		done <- (&Supervisor{LogDir: t.TempDir(), Progress: &syncBuffer{}}).Run(context.Background(), []Process{first, second})
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
	done := make(chan error, 1)
	go func() {
		done <- (&Supervisor{LogDir: t.TempDir(), Progress: &syncBuffer{}}).Run(context.Background(), []Process{
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
	progress := &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- (&Supervisor{LogDir: t.TempDir(), Progress: progress}).Run(context.Background(), []Process{
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
	if strings.Contains(progress.String(), "starting never") {
		t.Fatalf("later child was started:\n%s", progress.String())
	}
}

func TestRunKillsChildThatIgnoresTermination(t *testing.T) {
	shutdownGrace = 200 * time.Millisecond
	t.Cleanup(func() { shutdownGrace = 5 * time.Second })

	out := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (&Supervisor{LogDir: t.TempDir(), Verbose: true, Stdout: out, Stderr: out, Progress: out}).Run(ctx, []Process{
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

func TestRunSetsChildEnvironment(t *testing.T) {
	logDir := t.TempDir()
	done := make(chan error, 1)
	go func() {
		done <- (&Supervisor{LogDir: logDir, Progress: &syncBuffer{}}).Run(context.Background(), []Process{
			{
				Name: "envy",
				Path: "bash",
				Args: []string{"-c", "echo seen=$CORESHIFT_TEST_ONLY; exit 7"},
				Env:  append(os.Environ(), "CORESHIFT_TEST_ONLY=set"),
			},
		})
	}()

	err := waitForRun(t, done)
	if err == nil || !strings.Contains(err.Error(), "envy exited") {
		t.Fatalf("expected error naming the exited child, got: %v", err)
	}
	if log := readLogFile(t, logDir, "envy"); !strings.Contains(log, "seen=set") {
		t.Fatalf("child environment was not set, log:\n%s", log)
	}
}

func TestRunBeforeStartRunsBeforeProcess(t *testing.T) {
	gated := filepath.Join(t.TempDir(), "gated")
	out := &syncBuffer{}

	// The child only announces startup once the gate file exists, and the
	// hook is its only writer, so the announcement proves the hook ran to
	// completion before the child started.
	proc := Process{
		Name: "gated",
		Path: "bash",
		Args: []string{"-c", fmt.Sprintf(
			"while [ ! -f %q ]; do sleep 0.05; done; echo started; while :; do sleep 0.1; done", gated)},
		BeforeStart: func(ctx context.Context) error {
			return os.WriteFile(gated, []byte("ok"), 0o644)
		},
	}

	done := make(chan error, 1)
	go func() {
		done <- (&Supervisor{LogDir: t.TempDir(), Verbose: true, Stdout: out, Stderr: out, Progress: out}).Run(context.Background(), []Process{proc})
	}()

	waitForLine(t, out, "[gated] started")
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if err := waitForRun(t, done); err != nil {
		t.Fatalf("Run returned an error after SIGINT: %v", err)
	}
}

func TestRunBeforeStartFailureFailsRun(t *testing.T) {
	orderFile := filepath.Join(t.TempDir(), "order")
	out := &syncBuffer{}
	first := stopper("first", orderFile)
	second := stopper("second", orderFile)
	// The hook fails only after the earlier child announced startup, so
	// the teardown check below can rely on a clean termination.
	second.BeforeStart = func(ctx context.Context) error {
		waitForLine(t, out, "[first] started")
		return fmt.Errorf("gate never opened")
	}

	done := make(chan error, 1)
	go func() {
		done <- (&Supervisor{LogDir: t.TempDir(), Verbose: true, Stdout: out, Stderr: out, Progress: out}).Run(context.Background(), []Process{first, second})
	}()

	err := waitForRun(t, done)
	if err == nil || !strings.Contains(err.Error(), "before start of second") || !strings.Contains(err.Error(), "gate never opened") {
		t.Fatalf("expected error from failed BeforeStart hook, got: %v", err)
	}
	if got := readOrder(t, orderFile); !reflect.DeepEqual(got, []string{"first"}) {
		t.Fatalf("children were not torn down, termination order: %v", got)
	}
	if strings.Contains(out.String(), "[second] started") {
		t.Fatalf("child started despite hook failure:\n%s", out.String())
	}
}

func TestRunAfterStartGatesLaterChildren(t *testing.T) {
	orderFile := filepath.Join(t.TempDir(), "order")
	gated := filepath.Join(t.TempDir(), "gated")
	out := &syncBuffer{}

	// The gate file only appears once the AfterStart hook finishes, so the
	// later child starting proves the hook ran to completion first.
	first := stopper("first", orderFile)
	first.AfterStart = func(ctx context.Context) error {
		if _, err := os.Stat(filepath.Join(filepath.Dir(gated), "nowhere")); err == nil {
			return fmt.Errorf("unexpected file")
		}
		return os.WriteFile(gated, []byte("ok"), 0o644)
	}
	second := Process{
		Name: "second",
		Path: "bash",
		Args: []string{"-c", fmt.Sprintf(
			"while [ ! -f %q ]; do sleep 0.05; done; echo started; while :; do sleep 0.1; done", gated)},
	}

	done := make(chan error, 1)
	go func() {
		done <- (&Supervisor{LogDir: t.TempDir(), Verbose: true, Stdout: out, Stderr: out, Progress: out}).Run(context.Background(), []Process{first, second})
	}()

	waitForLine(t, out, "[second] started")
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if err := waitForRun(t, done); err != nil {
		t.Fatalf("Run returned an error after SIGINT: %v", err)
	}
}

func TestRunAfterStartFailureFailsRun(t *testing.T) {
	orderFile := filepath.Join(t.TempDir(), "order")
	out := &syncBuffer{}
	first := stopper("first", orderFile)
	// The hook fails only after the child installed its signal handler, so
	// the teardown check below can rely on a clean termination.
	first.AfterStart = func(ctx context.Context) error {
		waitForLine(t, out, "[first] started")
		return fmt.Errorf("gate never opened")
	}
	done := make(chan error, 1)
	go func() {
		done <- (&Supervisor{LogDir: t.TempDir(), Verbose: true, Stdout: out, Stderr: out, Progress: out}).Run(context.Background(), []Process{first, stopper("second", orderFile)})
	}()

	err := waitForRun(t, done)
	if err == nil || !strings.Contains(err.Error(), "after start of first") || !strings.Contains(err.Error(), "gate never opened") {
		t.Fatalf("expected error from failed AfterStart hook, got: %v", err)
	}
	if got := readOrder(t, orderFile); !reflect.DeepEqual(got, []string{"first"}) {
		t.Fatalf("children were not torn down, termination order: %v", got)
	}
	if strings.Contains(out.String(), "[second] started") {
		t.Fatalf("later child started despite hook failure:\n%s", out.String())
	}
}

func TestRunAfterStartHookRunsAfterAllChildren(t *testing.T) {
	progress := &syncBuffer{}
	hookRan := make(chan struct{})
	sup := &Supervisor{
		LogDir:   t.TempDir(),
		Progress: progress,
		AfterStart: func(ctx context.Context) error {
			close(hookRan)
			return nil
		},
	}
	done := make(chan error, 1)
	go func() {
		done <- sup.Run(context.Background(), []Process{
			stopper("only", filepath.Join(t.TempDir(), "unused")),
		})
	}()

	waitForLine(t, progress, "starting only")
	select {
	case <-hookRan:
	case <-time.After(10 * time.Second):
		t.Fatal("AfterStart hook did not run after all children started")
	}
	// Run keeps waiting for shutdown after the hook returned.
	select {
	case err := <-done:
		t.Fatalf("Run returned before shutdown was requested: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if err := waitForRun(t, done); err != nil {
		t.Fatalf("Run returned an error after SIGINT: %v", err)
	}
}

func TestRunAfterStartHookFailureFailsRun(t *testing.T) {
	orderFile := filepath.Join(t.TempDir(), "order")
	out := &syncBuffer{}
	// The hook fails only after the child installed its signal handler,
	// so the teardown check below can rely on a clean termination.
	sup := &Supervisor{
		LogDir:  t.TempDir(),
		Verbose: true,
		Stdout:  out,
		Stderr:  out,
		AfterStart: func(ctx context.Context) error {
			waitForLine(t, out, "[only] started")
			return fmt.Errorf("never ready")
		},
	}
	done := make(chan error, 1)
	go func() {
		done <- sup.Run(context.Background(), []Process{stopper("only", orderFile)})
	}()

	err := waitForRun(t, done)
	if err == nil || !strings.Contains(err.Error(), "never ready") {
		t.Fatalf("expected the hook's error, got: %v", err)
	}
	if got := readOrder(t, orderFile); !reflect.DeepEqual(got, []string{"only"}) {
		t.Fatalf("children were not torn down, termination order: %v", got)
	}
}

func TestRunAfterStartHookInterruptExitsCleanly(t *testing.T) {
	orderFile := filepath.Join(t.TempDir(), "order")
	out := &syncBuffer{}
	sup := &Supervisor{
		LogDir:   t.TempDir(),
		Verbose:  true,
		Stdout:   out,
		Stderr:   out,
		Progress: &syncBuffer{},
		// The hook polls until shutdown is requested, then reports the
		// cancellation as its error, the way a readiness poll does.
		AfterStart: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}
	done := make(chan error, 1)
	go func() {
		done <- sup.Run(context.Background(), []Process{stopper("only", orderFile)})
	}()

	waitForLine(t, out, "[only] started")
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if err := waitForRun(t, done); err != nil {
		t.Fatalf("Run returned an error after SIGINT during the hook: %v", err)
	}
	if got := readOrder(t, orderFile); !reflect.DeepEqual(got, []string{"only"}) {
		t.Fatalf("children were not torn down, termination order: %v", got)
	}
}

func TestRunChildAfterStartInterruptExitsCleanly(t *testing.T) {
	only := stopper("only", filepath.Join(t.TempDir(), "unused"))
	only.AfterStart = func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	progress := &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- (&Supervisor{LogDir: t.TempDir(), Progress: progress}).Run(context.Background(), []Process{only})
	}()

	waitForLine(t, progress, "starting only")
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if err := waitForRun(t, done); err != nil {
		t.Fatalf("Run returned an error after SIGINT during the hook: %v", err)
	}
}
