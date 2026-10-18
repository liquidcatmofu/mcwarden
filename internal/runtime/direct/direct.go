//go:build linux

// Package direct runs and observes a local Linux process through procfs.
//
// It implements runtime.Runtime without any container or Minecraft knowledge.
// Start launches a child process; State, Signal, and Wait keep targeting that
// same process generation. An already-running process can be adopted instead,
// in which case its PID and start time are recorded so a reused PID is never
// mistaken for the original.
package direct

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	hostlinux "github.com/liquidcatmofu/mcwarden/internal/host/linux"
	"github.com/liquidcatmofu/mcwarden/internal/runtime"
)

const adoptPollInterval = 100 * time.Millisecond

var (
	// ErrAlreadyRunning is returned when Start is called while a managed
	// process is still alive.
	ErrAlreadyRunning = errors.New("process is already running")
	// ErrNotStarted is returned when an operation needs a process but none is
	// under management.
	ErrNotStarted = errors.New("process is not running")
	// ErrUnsupported is returned for a signal the adapter cannot translate.
	ErrUnsupported = errors.New("unsupported operation")
)

// Adopt targets a process this Runtime did not start.
type Adopt struct {
	PID int
	// StartTime, when nonzero, must match /proc/<pid>/stat. It guards against a
	// reused PID being mistaken for the original process.
	StartTime uint64
	// SignalGroup allows signals to reach the adopted process group. It stays
	// off by default because the group may contain processes outside
	// MCWarden's control and should only be enabled after confirmation.
	SignalGroup bool
}

// Config describes how to launch and observe a server process. It is read-only
// after the Runtime is created.
type Config struct {
	Path string
	Args []string
	Dir  string
	// Env, when nil, inherits the current environment.
	Env []string

	// Stdin, Stdout, and Stderr are passed to the child. A nil value connects
	// that stream to the null device, matching os/exec.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// Adopt, when set, attaches to an already-running process instead of
	// launching one on first observation.
	Adopt *Adopt
}

// Runtime is a runtime.Runtime adapter for a local process.
type Runtime struct {
	cfg  Config
	mu   sync.Mutex
	proc *process
}

var _ runtime.Runtime = (*Runtime)(nil)

// New returns a Runtime for cfg. It does not launch anything; Start or a first
// observation does.
func New(cfg Config) *Runtime {
	return &Runtime{cfg: cfg}
}

// process is one process generation, either started by this Runtime or adopted.
type process struct {
	pid       int
	pgid      int
	startTime uint64
	owned     bool // started by this Runtime
	group     bool // signals may target the process group

	cmd     *exec.Cmd     // owned only
	done    chan struct{} // owned: closed once reaped
	exit    runtime.ExitStatus
	waitErr error
}

func (p *process) exited() bool {
	if p.owned {
		select {
		case <-p.done:
			return true
		default:
			return false
		}
	}
	return !p.adoptedAlive()
}

// adoptedAlive reports whether the adopted process still exists and still
// matches the recorded start time.
func (p *process) adoptedAlive() bool {
	stat, err := hostlinux.Read(p.pid)
	if err != nil {
		return false
	}
	return p.startTime == 0 || stat.StartTime == p.startTime
}

// resolve returns the managed process, adopting the configured PID on first
// use. A nil result means no process is running.
func (r *Runtime) resolve() (*process, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.proc != nil {
		return r.proc, nil
	}
	if r.cfg.Adopt == nil {
		return nil, nil
	}
	stat, err := hostlinux.Read(r.cfg.Adopt.PID)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("adopt pid %d: %w", r.cfg.Adopt.PID, err)
	}
	if r.cfg.Adopt.StartTime != 0 && stat.StartTime != r.cfg.Adopt.StartTime {
		// A different process now holds the PID; never target it.
		return nil, nil
	}
	r.proc = &process{
		pid:       stat.PID,
		pgid:      stat.PGID,
		startTime: stat.StartTime,
		group:     r.cfg.Adopt.SignalGroup,
	}
	return r.proc, nil
}

// Start launches the configured command in a new process group. Success means
// the process started, not that the server inside it is ready.
func (r *Runtime) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := r.resolve()
	if err != nil {
		return err
	}
	if current != nil && !current.exited() {
		return ErrAlreadyRunning
	}
	if r.cfg.Path == "" {
		return fmt.Errorf("start: command path is empty: %w", ErrUnsupported)
	}

	cmd := exec.Command(r.cfg.Path, r.cfg.Args...)
	cmd.Dir = r.cfg.Dir
	cmd.Env = r.cfg.Env
	cmd.Stdin = r.cfg.Stdin
	cmd.Stdout = r.cfg.Stdout
	cmd.Stderr = r.cfg.Stderr
	// A new process group lets Signal reach wrapper scripts and their children
	// instead of only the direct child.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start process: %w", err)
	}
	next := &process{
		pid:   cmd.Process.Pid,
		pgid:  cmd.Process.Pid,
		owned: true,
		group: true,
		cmd:   cmd,
		done:  make(chan struct{}),
	}
	if stat, err := hostlinux.Read(next.pid); err == nil {
		next.startTime = stat.StartTime
		next.pgid = stat.PGID
	}
	go next.reap()
	r.mu.Lock()
	r.proc = next
	r.mu.Unlock()
	return nil
}

// reap records the exit status of an owned process and closes its done channel.
// A nonzero exit code is not an error; only a failure to observe the process is.
func (p *process) reap() {
	exit := runtime.ExitStatus{Known: true}
	if err := p.cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				exit.Code = -1
				exit.Signaled = true
				exit.Signal = mapSignal(status.Signal())
			} else {
				exit.Code = exitErr.ExitCode()
			}
		} else {
			exit.Known = false
			p.waitErr = err
		}
	}
	p.exit = exit
	close(p.done)
}

// State reports Running only while the managed process generation is alive.
func (r *Runtime) State(ctx context.Context) (runtime.State, error) {
	if err := ctx.Err(); err != nil {
		return runtime.Unknown, err
	}
	p, err := r.resolve()
	if err != nil {
		return runtime.Unknown, err
	}
	if p == nil || p.exited() {
		return runtime.Stopped, nil
	}
	return runtime.Running, nil
}

// Signal delivers a signal to the current generation. Owned processes are
// signalled through their process group; adopted ones only reach the group when
// explicitly allowed.
func (r *Runtime) Signal(ctx context.Context, sig runtime.Signal) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	native, err := nativeSignal(sig)
	if err != nil {
		return err
	}
	p, err := r.resolve()
	if err != nil {
		return err
	}
	if p == nil || p.exited() {
		return ErrNotStarted
	}
	target := p.pid
	if p.owned || p.group {
		target = -p.pgid
	}
	if err := syscall.Kill(target, native); err != nil {
		return fmt.Errorf("signal %s: %w", sig, err)
	}
	return nil
}

// Wait blocks until the current generation exits or ctx is done. It is safe to
// retry after a timeout: the same generation is observed again, and an owned
// process returns its stored exit status once reaped.
func (r *Runtime) Wait(ctx context.Context) (runtime.ExitStatus, error) {
	p, err := r.resolve()
	if err != nil {
		return runtime.ExitStatus{}, err
	}
	if p == nil {
		return runtime.ExitStatus{}, ErrNotStarted
	}
	if p.owned {
		select {
		case <-p.done:
			return p.exit, p.waitErr
		case <-ctx.Done():
			return runtime.ExitStatus{}, ctx.Err()
		}
	}

	ticker := time.NewTicker(adoptPollInterval)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return runtime.ExitStatus{}, err
		}
		if !p.adoptedAlive() {
			// An adopted process is not a child, so no exit status is available.
			return runtime.ExitStatus{Known: false}, nil
		}
		select {
		case <-ctx.Done():
			return runtime.ExitStatus{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func nativeSignal(sig runtime.Signal) (syscall.Signal, error) {
	switch sig {
	case runtime.Terminate:
		return syscall.SIGTERM, nil
	case runtime.Kill:
		return syscall.SIGKILL, nil
	default:
		return 0, fmt.Errorf("signal %q: %w", sig, ErrUnsupported)
	}
}

func mapSignal(sig syscall.Signal) runtime.Signal {
	switch sig {
	case syscall.SIGTERM:
		return runtime.Terminate
	case syscall.SIGKILL:
		return runtime.Kill
	default:
		return ""
	}
}
