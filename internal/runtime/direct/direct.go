//go:build linux

// Package direct runs and observes a local Linux process through procfs.
//
// It implements runtime.Runtime without any container or Minecraft knowledge.
// Start launches a child process; State, Signal, and Wait keep targeting that
// same process generation. An already-running process can be adopted instead,
// in which case its PID and start time are recorded so a reused PID is never
// mistaken for the original.
//
// DirectRuntime tracks exactly one process and does not walk the process tree.
// The configured command must therefore be the server JVM itself, or a wrapper
// whose final step replaces itself with the JVM (for example ending in
// "exec java ..."). A wrapper that spawns the JVM and exits first makes Wait
// report process exit while the JVM may still be running, and a wrapper that
// outlives the JVM keeps State reporting Running; neither matches the Runtime
// contract of tracking the JVM.
package direct

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"

	hostlinux "github.com/liquidcatmofu/mcwarden/internal/host/linux"
	"github.com/liquidcatmofu/mcwarden/internal/runtime"
)

const adoptPollInterval = 100 * time.Millisecond

// procRead reads a process snapshot. It is indirected so tests can inject
// procfs failures.
var procRead = hostlinux.Read

var (
	// ErrAlreadyRunning is returned when Start is called while a managed
	// process is still alive.
	ErrAlreadyRunning = errors.New("process is already running")
	// ErrNotStarted is returned when an operation needs a process but none is
	// under management.
	ErrNotStarted = errors.New("process is not running")
	// ErrUnsupported is returned for a signal the adapter cannot translate.
	ErrUnsupported = errors.New("unsupported operation")
	// ErrInvalid is returned for an unusable configuration.
	ErrInvalid = errors.New("invalid configuration")
	// ErrIdentityChanged is returned when procfs no longer matches the PID,
	// start time, or process group recorded for a generation, for example
	// because the PID was reused or the process moved to another group.
	ErrIdentityChanged = errors.New("process identity changed")
)

// Adopt targets a process this Runtime did not start.
type Adopt struct {
	PID int
	// StartTime is the /proc/<pid>/stat start time recorded when the PID was
	// observed. It is required; it guards against a reused PID being mistaken
	// for the original process. Callers that only know a PID should read its
	// start time and construct Adopt from that PID and start time together.
	StartTime uint64
	// PGID is the process group recorded with the identity. When nonzero, a
	// process found in a different group is rejected as a changed identity. It
	// may be zero when only a PID and start time are known, in which case the
	// group is read from procfs on adoption.
	PGID int
	// Owned marks a process that a previous MCWarden invocation started and
	// persisted, as opposed to one discovered through outside means such as a
	// pidfile. It makes signals target the process group and preserves the
	// persisted ownership bit. It does not make the process a child of this
	// invocation, so Wait still cannot observe its exit status.
	Owned bool
	// SignalGroup allows signals to reach the adopted process group. It stays
	// off by default because the group may contain processes outside
	// MCWarden's control and should only be enabled after confirmation.
	SignalGroup bool
}

// Config describes how to launch and observe a server process. It is read-only
// after the Runtime is created.
type Config struct {
	// Path is the server JVM or a wrapper that ends by exec-ing the JVM. See
	// the package comment for why a wrapper must not outlive the JVM.
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
var _ runtime.Identifiable = (*Runtime)(nil)

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
	// owned is true for a process managed by MCWarden, including one adopted
	// from persisted state that a previous invocation started. It selects group
	// signalling and the ownership bit in the identity.
	owned bool
	group bool // signals may target the process group

	// cmd and done are set only when this Runtime started the process. A
	// rehydrated owned process is managed but not a child, so it cannot be
	// wait(2)ed and its exit status is unknown.
	cmd     *exec.Cmd
	done    chan struct{}
	exit    runtime.ExitStatus
	waitErr error
}

// gone reports whether the managed process is no longer running. A waitable
// child uses its reaped state. Anything else reads procfs and may return an
// error; callers must not treat an unreadable process as gone.
func (p *process) gone() (bool, error) {
	if p.done != nil {
		select {
		case <-p.done:
			return true, nil
		default:
			return false, nil
		}
	}
	alive, err := p.procAlive()
	if err != nil {
		return false, err
	}
	return !alive, nil
}

// procAlive reports whether the recorded process generation still exists,
// matches the recorded start time and process group, and is not a zombie. Only
// a missing process (ENOENT/ESRCH), a start-time mismatch, or a zombie/dead
// state counts as not alive; any other procfs error is returned so the caller
// never mistakes an unreadable process for a stopped one. A process that moved
// groups is an identity change, not a stopped process.
func (p *process) procAlive() (bool, error) {
	stat, err := procRead(p.pid)
	if hostlinux.IsGone(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if stat.StartTime != p.startTime {
		return false, nil
	}
	switch stat.State {
	case 'Z', 'X':
		return false, nil
	}
	if p.pgid != 0 && stat.PGID != p.pgid {
		return false, fmt.Errorf("process %d moved to process group %d: %w", p.pid, stat.PGID, ErrIdentityChanged)
	}
	return true, nil
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
	if r.cfg.Adopt.StartTime == 0 {
		return nil, fmt.Errorf("adopt requires a start time: %w", ErrInvalid)
	}
	stat, err := procRead(r.cfg.Adopt.PID)
	if hostlinux.IsGone(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("adopt pid %d: %w", r.cfg.Adopt.PID, err)
	}
	if stat.StartTime != r.cfg.Adopt.StartTime {
		// A different process now holds the PID; never target it.
		return nil, nil
	}
	if r.cfg.Adopt.PGID != 0 && stat.PGID != r.cfg.Adopt.PGID {
		return nil, fmt.Errorf("pid %d is in process group %d, recorded %d: %w",
			stat.PID, stat.PGID, r.cfg.Adopt.PGID, ErrIdentityChanged)
	}
	r.proc = &process{
		pid:       stat.PID,
		pgid:      stat.PGID,
		startTime: stat.StartTime,
		owned:     r.cfg.Adopt.Owned,
		group:     r.cfg.Adopt.Owned || r.cfg.Adopt.SignalGroup,
	}
	return r.proc, nil
}

// Identity reports the generation this Runtime is already tracking. It returns
// false when there is no tracked generation or its start time could not be
// observed. Reporting an identity never launches or adopts a process; call
// State, Start, Signal, or Wait first to establish the generation.
func (r *Runtime) Identity() (runtime.Identity, bool) {
	r.mu.Lock()
	p := r.proc
	r.mu.Unlock()
	if p == nil {
		return runtime.Identity{}, false
	}
	if p.done != nil {
		select {
		case <-p.done:
			return runtime.Identity{}, false
		default:
		}
	} else {
		alive, err := p.procAlive()
		if err != nil || !alive {
			return runtime.Identity{}, false
		}
	}
	if p.startTime == 0 {
		stat, err := procRead(p.pid)
		if err != nil {
			return runtime.Identity{}, false
		}
		p.startTime = stat.StartTime
		p.pgid = stat.PGID
	}
	if p.startTime == 0 {
		return runtime.Identity{}, false
	}
	return runtime.Identity{
		PID:         p.pid,
		PGID:        p.pgid,
		StartTime:   p.startTime,
		Owned:       p.owned,
		SignalGroup: p.group,
	}, true
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
	if current != nil {
		gone, err := current.gone()
		if err != nil {
			return err
		}
		if !gone {
			return ErrAlreadyRunning
		}
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
	if stat, err := procRead(next.pid); err == nil {
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
				exit.Signal = &runtime.ExitSignal{Number: int(status.Signal()), Name: signalName(status.Signal())}
				exit.CoreDumped = status.CoreDump()
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
	if p == nil {
		return runtime.Stopped, nil
	}
	gone, err := p.gone()
	if err != nil {
		return runtime.Unknown, err
	}
	if gone {
		return runtime.Stopped, nil
	}
	return runtime.Running, nil
}

// Signal delivers a signal to the current generation. Owned processes are
// signalled through their process group; adopted ones only reach the group when
// explicitly allowed. Immediately before signalling, the PID, start time, and
// process group are re-read from procfs so a reused PID or a moved process is
// never targeted.
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
	if p == nil {
		return ErrNotStarted
	}
	if err := r.verifySignal(p); err != nil {
		return err
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

// verifySignal re-reads procfs right before a signal. Group signalling also
// requires the process to still lead that group, so an adopted PID that merely
// belongs to someone else's group is never used as a group handle. Callers
// serialize lifecycle operations, so updating an unidentified start time here
// does not race with another Signal.
func (r *Runtime) verifySignal(p *process) error {
	if p.done != nil {
		select {
		case <-p.done:
			return ErrNotStarted
		default:
		}
	}
	stat, err := procRead(p.pid)
	if hostlinux.IsGone(err) {
		return ErrNotStarted
	}
	if err != nil {
		return fmt.Errorf("verify process %d: %w", p.pid, err)
	}
	switch stat.State {
	case 'Z', 'X':
		return ErrNotStarted
	}
	if p.startTime == 0 {
		p.startTime = stat.StartTime
	} else if stat.StartTime != p.startTime {
		return fmt.Errorf("pid %d now belongs to a different process: %w", p.pid, ErrIdentityChanged)
	}
	if p.pgid == 0 {
		p.pgid = stat.PGID
	} else if stat.PGID != p.pgid {
		return fmt.Errorf("process %d moved to process group %d: %w", p.pid, stat.PGID, ErrIdentityChanged)
	}
	if p.owned || p.group {
		if p.pgid != p.pid {
			return fmt.Errorf("process %d is not a process group leader (pgid %d): %w", p.pid, p.pgid, ErrIdentityChanged)
		}
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
	if p.done != nil {
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
		alive, err := p.procAlive()
		if err != nil {
			return runtime.ExitStatus{}, err
		}
		if !alive {
			// A rehydrated or adopted process is not a child of this
			// invocation, so no exit status is available.
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

// signalNames maps terminating signals to their conventional names. Exit
// causes are intentionally a wider set than the operation signals above, so a
// crash is not silently reduced to SIGTERM or SIGKILL.
var signalNames = map[syscall.Signal]string{
	syscall.SIGHUP:    "SIGHUP",
	syscall.SIGINT:    "SIGINT",
	syscall.SIGQUIT:   "SIGQUIT",
	syscall.SIGILL:    "SIGILL",
	syscall.SIGTRAP:   "SIGTRAP",
	syscall.SIGABRT:   "SIGABRT",
	syscall.SIGBUS:    "SIGBUS",
	syscall.SIGFPE:    "SIGFPE",
	syscall.SIGKILL:   "SIGKILL",
	syscall.SIGUSR1:   "SIGUSR1",
	syscall.SIGSEGV:   "SIGSEGV",
	syscall.SIGUSR2:   "SIGUSR2",
	syscall.SIGPIPE:   "SIGPIPE",
	syscall.SIGALRM:   "SIGALRM",
	syscall.SIGTERM:   "SIGTERM",
	syscall.SIGCHLD:   "SIGCHLD",
	syscall.SIGCONT:   "SIGCONT",
	syscall.SIGSTOP:   "SIGSTOP",
	syscall.SIGTSTP:   "SIGTSTP",
	syscall.SIGTTIN:   "SIGTTIN",
	syscall.SIGTTOU:   "SIGTTOU",
	syscall.SIGURG:    "SIGURG",
	syscall.SIGXCPU:   "SIGXCPU",
	syscall.SIGXFSZ:   "SIGXFSZ",
	syscall.SIGVTALRM: "SIGVTALRM",
	syscall.SIGPROF:   "SIGPROF",
	syscall.SIGWINCH:  "SIGWINCH",
	syscall.SIGIO:     "SIGIO",
	syscall.SIGPWR:    "SIGPWR",
	syscall.SIGSYS:    "SIGSYS",
}

func signalName(sig syscall.Signal) string {
	return signalNames[sig]
}
