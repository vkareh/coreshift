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

	for i := range processes {
		c, err := startChild(&processes[i], stdout, stderr, &mu)
		if err != nil {
			shutdown()
			return fmt.Errorf("start %s: %w", processes[i].Name, err)
		}
		children = append(children, c)
	}

	exits := make(chan *child, len(children))
	for _, c := range children {
		go func() {
			c.err = c.cmd.Wait()
			close(c.done)
			exits <- c
		}()
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
