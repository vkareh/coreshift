// Package supervisor runs child processes for the lifetime of a CoreShift
// API invocation.
package supervisor

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// shutdownGrace is how long a child has to exit after a termination signal
// before it is killed. It is a variable so tests can shorten it.
var shutdownGrace = 5 * time.Second

// logTailLines is how many lines of a child's log file a failure error
// includes.
const logTailLines = 20

// Process describes a child process managed by a Supervisor.
type Process struct {
	// Name identifies the process in its log file, in forwarded output,
	// and in errors.
	Name string
	// Path is the executable to run.
	Path string
	// Args are the arguments passed to the executable.
	Args []string
	// Env, when set, is the complete environment for the child, replacing
	// this process's environment. When nil the child inherits it.
	Env []string
	// BeforeStart, when set, runs right before this process starts, after
	// all earlier processes and their hooks have completed. It is how a
	// process consumes state that only exists once earlier processes are
	// running, for example by reading objects the API server serves.
	// Returning an error fails the run and tears the children down.
	BeforeStart func(ctx context.Context) error
	// AfterStart, when set, runs after this process starts and must return
	// before later processes start. It is how a process that other children
	// depend on gates their startup, for example by waiting for the process
	// to become ready and seeding anything the dependents need. Returning
	// an error fails the run and tears the children down.
	AfterStart func(ctx context.Context) error
}

// Supervisor runs a set of child processes until shutdown is requested.
type Supervisor struct {
	// LogDir is the directory each child's combined output is captured to,
	// one log file per child. It is created when missing.
	LogDir string
	// Verbose also forwards each child's output live to Stdout and Stderr
	// with name prefixes.
	Verbose bool
	// Stdout and Stderr receive verbose output, prefixed with the child's
	// name so interleaved logs stay attributable. They default to os.Stdout
	// and os.Stderr.
	Stdout io.Writer
	Stderr io.Writer
	// Progress receives one line when each child starts and stops. It
	// defaults to os.Stdout.
	Progress io.Writer
	// AfterStart, when set, runs once every process has started, before
	// Run settles into waiting for shutdown or failure. It is where a run
	// waits for readiness and reports success. Returning an error fails
	// the run and tears the children down.
	AfterStart func(ctx context.Context) error
}

// child is a started process together with the outcome of waiting for it.
type child struct {
	proc    *Process
	cmd     *exec.Cmd
	logFile *os.File
	logPath string
	copiers sync.WaitGroup
	done    chan struct{}
	err     error
}

// Run starts the given processes in order and blocks until the context is
// canceled, SIGINT or SIGTERM arrives, or one of the processes exits. On
// shutdown the children are terminated in reverse startup order. If a
// process exits before shutdown was requested, the remaining children are
// torn down and Run returns an error naming the failed component, its
// recent log output, and its log file.
func (s *Supervisor) Run(ctx context.Context, processes []Process) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	stdout, stderr := s.Stdout, s.Stderr
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	if err := os.MkdirAll(s.LogDir, 0o755); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}

	var mu sync.Mutex
	var children []*child
	shutdown := func() {
		for i := len(children) - 1; i >= 0; i-- {
			s.progress("stopping " + children[i].proc.Name)
			children[i].terminate()
		}
	}

	// Exit reports are collected from the moment each child starts, so a
	// shutdown partway through the startup loop can still wait for children
	// that have already been started.
	exits := make(chan *child, len(processes))
	waitForExit := func(c *child) {
		go func() {
			c.err = c.cmd.Wait()
			// The log file closes once its copiers drained the child's
			// final output, so a failure error read from it sees every
			// line.
			c.copiers.Wait()
			c.logFile.Close()
			close(c.done)
			exits <- c
		}()
	}

	for i := range processes {
		if hook := processes[i].BeforeStart; hook != nil {
			if err := hook(ctx); err != nil {
				shutdown()
				// A canceled context means shutdown was requested while
				// the hook ran; report a clean shutdown, not a hook
				// failure.
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("before start of %s: %w", processes[i].Name, err)
			}
			if ctx.Err() != nil {
				shutdown()
				return nil
			}
		}
		s.progress("starting " + processes[i].Name)
		c, err := s.startChild(&processes[i], stdout, stderr, &mu)
		if err != nil {
			shutdown()
			return fmt.Errorf("start %s: %w", processes[i].Name, err)
		}
		children = append(children, c)
		waitForExit(c)
		if hook := processes[i].AfterStart; hook != nil {
			if err := hook(ctx); err != nil {
				shutdown()
				if ctx.Err() != nil {
					return nil
				}
				return c.withLog(fmt.Errorf("after start of %s: %w", processes[i].Name, err))
			}
			if ctx.Err() != nil {
				shutdown()
				return nil
			}
		}
	}

	if s.AfterStart != nil {
		if err := s.AfterStart(ctx); err != nil {
			shutdown()
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if ctx.Err() != nil {
			shutdown()
			return nil
		}
	}

	select {
	case <-ctx.Done():
		shutdown()
		return nil
	case c := <-exits:
		shutdown()
		return c.failure()
	}
}

// failure describes why the child ended the run.
func (c *child) failure() error {
	if c.err != nil {
		return c.withLog(fmt.Errorf("%s exited: %w", c.proc.Name, c.err))
	}
	return c.withLog(fmt.Errorf("%s exited unexpectedly", c.proc.Name))
}

// withLog appends the child's recent log output and log file path to err,
// so a failure can be debugged without rerunning.
func (c *child) withLog(err error) error {
	tail, tailErr := tailFile(c.logPath, logTailLines)
	if tailErr != nil || tail == "" {
		return err
	}
	return fmt.Errorf("%w\n\nrecent log output:\n%s\n\nfull log: %s", err, tail, c.logPath)
}

// tailFile returns the last lines of a file.
func tailFile(path string, lines int) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	all := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n"), nil
}

// terminate asks the child to exit and waits for it, killing it if it does
// not exit within the shutdown grace period.
func (c *child) terminate() {
	_ = c.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-c.done:
		return
	case <-time.After(shutdownGrace):
		_ = c.cmd.Process.Kill()
		<-c.done
	}
}

// progress prints one lifecycle line.
func (s *Supervisor) progress(line string) {
	w := s.Progress
	if w == nil {
		w = os.Stdout
	}
	fmt.Fprintln(w, line)
}

func (s *Supervisor) startChild(proc *Process, stdout, stderr io.Writer, mu *sync.Mutex) (*child, error) {
	cmd := exec.Command(proc.Path, proc.Args...)
	cmd.Env = proc.Env

	logPath := filepath.Join(s.LogDir, proc.Name+".log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, fmt.Errorf("create log file: %w", err)
	}

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		logFile.Close()
		return nil, err
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		logFile.Close()
		stdoutR.Close()
		stdoutW.Close()
		return nil, err
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW

	if err := cmd.Start(); err != nil {
		logFile.Close()
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		return nil, err
	}
	// Children inherit the write ends; close this process's copies so the
	// copy goroutines see EOF once a child exits.
	stdoutW.Close()
	stderrW.Close()

	// Verbose runs forward each line live with a name prefix, the way
	// every run did before log files; quiet runs only write the file.
	var liveOut, liveErr io.Writer
	if s.Verbose {
		liveOut, liveErr = stdout, stderr
	}

	c := &child{proc: proc, cmd: cmd, logFile: logFile, logPath: logPath, done: make(chan struct{})}
	c.copiers.Add(2)
	go func() {
		defer c.copiers.Done()
		copyLines(stdoutR, proc.Name, logFile, liveOut, mu)
	}()
	go func() {
		defer c.copiers.Done()
		copyLines(stderrR, proc.Name, logFile, liveErr, mu)
	}()
	return c, nil
}

// copyLines writes each line read from r to the child's log file and, when
// live is set, to it with a name prefix.
func copyLines(r io.ReadCloser, name string, log io.Writer, live io.Writer, mu *sync.Mutex) {
	defer r.Close()
	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			mu.Lock()
			if live != nil {
				_, _ = fmt.Fprintf(live, "[%s] %s\n", name, strings.TrimSuffix(line, "\n"))
			}
			_, _ = fmt.Fprint(log, line)
			mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}
