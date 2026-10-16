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
	"strings"
	"sync"
	"syscall"
	"time"
)

// shutdownGrace is how long a child has to exit after a termination signal
// before it is killed. It is a variable so tests can shorten it.
var shutdownGrace = 5 * time.Second

// Process describes a child process managed by a Supervisor.
type Process struct {
	// Name identifies the process in forwarded output and in errors.
	Name string
	// Path is the executable to run.
	Path string
	// Args are the arguments passed to the executable.
	Args []string
	// AfterStart, when set, runs after this process starts and must return
	// before later processes start. It is how a process that other children
	// depend on gates their startup, for example by waiting for the process
	// to become ready and seeding anything the dependents need. Returning an
	// error fails the run and tears the children down.
	AfterStart func(ctx context.Context) error
}

// Supervisor runs a set of child processes until shutdown is requested.
type Supervisor struct {
	// Stdout and Stderr receive each child's output, prefixed with the
	// child's name so interleaved logs stay attributable. They default to
	// os.Stdout and os.Stderr.
	Stdout io.Writer
	Stderr io.Writer
}

// child is a started process together with the outcome of waiting for it.
type child struct {
	proc *Process
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

// Run starts the given processes in order and blocks until the context is
// canceled, SIGINT or SIGTERM arrives, or one of the processes exits. On
// shutdown the children are terminated in reverse startup order. If a
// process exits before shutdown was requested, the remaining children are
// torn down and Run returns an error naming the failed component.
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

	var mu sync.Mutex
	var children []*child
	shutdown := func() {
		for i := len(children) - 1; i >= 0; i-- {
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
			close(c.done)
			exits <- c
		}()
	}

	for i := range processes {
		c, err := startChild(&processes[i], stdout, stderr, &mu)
		if err != nil {
			shutdown()
			return fmt.Errorf("start %s: %w", processes[i].Name, err)
		}
		children = append(children, c)
		waitForExit(c)
		if hook := processes[i].AfterStart; hook != nil {
			if err := hook(ctx); err != nil {
				shutdown()
				return fmt.Errorf("after start of %s: %w", processes[i].Name, err)
			}
			// A canceled context means shutdown was requested while the
			// hook ran; report a clean shutdown, not a hook failure.
			if ctx.Err() != nil {
				shutdown()
				return nil
			}
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
		return fmt.Errorf("%s exited: %w", c.proc.Name, c.err)
	}
	return fmt.Errorf("%s exited unexpectedly", c.proc.Name)
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

func startChild(proc *Process, stdout, stderr io.Writer, mu *sync.Mutex) (*child, error) {
	cmd := exec.Command(proc.Path, proc.Args...)

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdoutR.Close()
		stdoutW.Close()
		return nil, err
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW

	if err := cmd.Start(); err != nil {
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

	c := &child{proc: proc, cmd: cmd, done: make(chan struct{})}
	go copyLines(stdoutR, proc.Name, stdout, mu)
	go copyLines(stderrR, proc.Name, stderr, mu)
	return c, nil
}

// copyLines forwards each line read from r to w with a name prefix.
func copyLines(r io.ReadCloser, name string, w io.Writer, mu *sync.Mutex) {
	defer r.Close()
	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			mu.Lock()
			_, _ = fmt.Fprintf(w, "[%s] %s\n", name, strings.TrimSuffix(line, "\n"))
			mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}
