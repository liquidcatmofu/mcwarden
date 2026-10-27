package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/liquidcatmofu/mcwarden/internal/minecraft"
	"github.com/liquidcatmofu/mcwarden/internal/runtime"
	"github.com/liquidcatmofu/mcwarden/internal/state"
)

var errWaitTimeout = errors.New("lifecycle wait deadline exceeded")

type Escalation int

const (
	StopOnly Escalation = iota
	WithTerm
	WithKill
)

type StopOptions struct {
	Timeout time.Duration
}

type RestartOptions struct {
	StartTimeout time.Duration
	NoWait       bool
	StopTimeout  time.Duration
	TermTimeout  time.Duration
	KillTimeout  time.Duration
	Escalation   Escalation
	AllowSIGKILL bool
}

type Event string

const (
	StopRequested    Event = "stop_requested"
	StopTimedOut     Event = "stop_timed_out"
	TermSent         Event = "term_sent"
	TermTimedOut     Event = "term_timed_out"
	KillSent         Event = "kill_sent"
	KillTimedOut     Event = "kill_timed_out"
	Stopped          Event = "stopped"
	StartRequested   Event = "start_requested"
	ProcessStarted   Event = "process_started"
	StartupCompleted Event = "startup_completed"
	StartupTimedOut  Event = "startup_timed_out"
	StartupExited    Event = "startup_exited"
)

// Result keeps operation events separate from the last observed lifecycle state.
// A timeout does not establish process exit or cancel server startup.
type Result struct {
	Events []Event             `json:"events"`
	State  minecraft.State     `json:"state"`
	Exit   *runtime.ExitStatus `json:"exit,omitempty"`
}

type Service struct {
	Runtime   runtime.Runtime
	Minecraft minecraft.Control
	// RuntimeName is stored in state, for example "direct".
	RuntimeName string
	// StateStore opens the persisted identity store. Bootstrap sets it; a nil
	// value disables persistence for tests and runtimes without a host process.
	StateStore func() (*state.Store, error)
}

func (s Service) requireRunning(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.Runtime == nil {
		return ErrUnsupported
	}
	state, err := s.Runtime.State(ctx)
	if err != nil {
		return fmt.Errorf("runtime state: %w", err)
	}
	if state == runtime.Stopped {
		return ErrNotRunning
	}
	if state != runtime.Running {
		return fmt.Errorf("runtime state is %q", state)
	}
	return nil
}

func (s Service) graceful(ctx context.Context, timeout time.Duration, result *Result) error {
	if err := s.requireRunning(ctx); err != nil {
		return err
	}
	if s.Minecraft == nil {
		return ErrUnsupported
	}
	caps, err := s.Minecraft.Capabilities(ctx)
	if err != nil {
		return fmt.Errorf("minecraft capabilities: %w", err)
	}
	if !caps.Command {
		return ErrUnsupported
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.Minecraft.Command(ctx, "stop"); err != nil {
		return fmt.Errorf("minecraft stop: %w", err)
	}
	result.Events = append(result.Events, StopRequested)
	result.State = minecraft.Stopping
	return s.wait(ctx, timeout, result, StopTimedOut)
}

func (s Service) wait(ctx context.Context, timeout time.Duration, result *Result, timedOut Event) error {
	child, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	exit, err := s.Runtime.Wait(child)
	// Parent cancellation/deadline is never a reason to escalate.
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && child.Err() == context.DeadlineExceeded {
			result.Events = append(result.Events, timedOut)
			return fmt.Errorf("%s: %w", timedOut, errors.Join(ErrTimeout, errWaitTimeout))
		}
		return fmt.Errorf("wait for exit: %w", err)
	}
	result.Events = append(result.Events, Stopped)
	result.State = minecraft.Stopped
	result.Exit = &exit
	return nil
}

// Start validates persisted state, locks the server, launches the process, and
// persists the new generation's identity. It returns after process launch; it
// does not wait for Minecraft readiness.
func (s Service) Start(ctx context.Context) (Result, error) {
	result := Result{Events: []Event{}, State: minecraft.Unknown}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	result.Events = append(result.Events, StartRequested)
	if err := s.startProcess(ctx); err != nil {
		return result, err
	}
	result.Events = append(result.Events, ProcessStarted)
	result.State = minecraft.Starting
	return result, nil
}

// startProcess is shared by Start and Restart. When state is configured it
// holds the server lock across validation, launch, and save:
//
//   - managed state means the process is still the recorded generation;
//   - stale state must be cleaned explicitly;
//   - unknown state is never treated as stopped;
//   - absent state still checks the runtime, which covers pidfile adoption.
//
// If the identity cannot be saved after launch, the new process is stopped
// again rather than left untracked, because an untracked process would allow a
// later invocation to start a second one.
func (s Service) startProcess(ctx context.Context) error {
	if s.Runtime == nil {
		return ErrUnsupported
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.StateStore == nil {
		return s.Runtime.Start(ctx)
	}
	store, err := s.StateStore()
	if err != nil {
		return fmt.Errorf("state: %w", err)
	}
	lock, err := store.Lock()
	if err != nil {
		return fmt.Errorf("state lock: %w", err)
	}
	defer lock.Unlock()

	inspection, err := store.Inspect()
	if err != nil {
		return fmt.Errorf("state: %w", err)
	}
	switch inspection.Status {
	case state.Managed:
		return fmt.Errorf("%w: %s", ErrAlreadyRunning, inspection.Reason)
	case state.Unknown:
		return fmt.Errorf("%w: %s", ErrStateUnknown, inspection.Reason)
	case state.Stale:
		return fmt.Errorf("%w: %s; run mcwarden state clean", ErrStaleState, inspection.Reason)
	}

	runtimeState, err := s.Runtime.State(ctx)
	if err != nil {
		return fmt.Errorf("runtime state: %w", err)
	}
	switch runtimeState {
	case runtime.Stopped:
	case runtime.Running:
		return fmt.Errorf("%w: runtime reports a running process", ErrAlreadyRunning)
	default:
		return fmt.Errorf("cannot start while runtime state is %q: %w", runtimeState, ErrStateUnknown)
	}
	if err := s.Runtime.Start(ctx); err != nil {
		return fmt.Errorf("start: %w", err)
	}

	identity, ok := s.identity()
	if !ok {
		rollbackErr := s.rollbackStart(ctx)
		return fmt.Errorf("%w: runtime did not report process identity (rollback: %v)", ErrStateUnknown, rollbackErr)
	}
	runtimeName := s.RuntimeName
	if runtimeName == "" {
		runtimeName = "direct"
	}
	if err := store.Save(runtimeName, identity); err != nil {
		rollbackErr := s.rollbackStart(ctx)
		return fmt.Errorf("save process state: %w (rollback: %v)", err, rollbackErr)
	}
	return nil
}

func (s Service) identity() (state.Identity, bool) {
	identifiable, ok := s.Runtime.(runtime.Identifiable)
	if !ok {
		return state.Identity{}, false
	}
	id, ok := identifiable.Identity()
	if !ok {
		return state.Identity{}, false
	}
	return state.Identity{
		PID:         id.PID,
		PGID:        id.PGID,
		StartTime:   id.StartTime,
		Owned:       id.Owned,
		SignalGroup: id.SignalGroup,
	}, true
}

// rollbackStart stops a freshly started process whose identity could not be
// persisted. It uses its own deadline so caller cancellation cannot leave the
// process untracked.
func (s Service) rollbackStart(ctx context.Context) error {
	if ctx.Err() != nil {
		ctx = context.Background()
	}
	child, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	signalErr := s.Runtime.Signal(child, runtime.Kill)
	_, waitErr := s.Runtime.Wait(child)
	return errors.Join(signalErr, waitErr)
}

// Stop never sends OS signals, even on timeout or transport failure.
func (s Service) Stop(ctx context.Context, opts StopOptions) (Result, error) {
	result := Result{Events: []Event{}, State: minecraft.Unknown}
	if opts.Timeout <= 0 {
		return result, fmt.Errorf("stop timeout must be positive: %w", ErrInvalid)
	}
	err := s.graceful(ctx, opts.Timeout, &result)
	return result, err
}

// Restart escalates only after its own wait timeout, never after an arbitrary
// error. Start is called only after Wait positively confirms the old process exited.
func (s Service) Restart(ctx context.Context, opts RestartOptions) (Result, error) {
	result := Result{Events: []Event{}, State: minecraft.Unknown}
	if (!opts.NoWait && opts.StartTimeout <= 0) || opts.StopTimeout <= 0 || opts.Escalation < StopOnly || opts.Escalation > WithKill ||
		(opts.Escalation >= WithTerm && opts.TermTimeout <= 0) ||
		(opts.Escalation == WithKill && (opts.KillTimeout <= 0 || !opts.AllowSIGKILL)) {
		return result, fmt.Errorf("invalid restart timeout or escalation authorization: %w", ErrInvalid)
	}

	// Check readiness support before stopping a working server.
	if err := ctx.Err(); err != nil {
		return result, err
	}
	var observer minecraft.Observer
	if !opts.NoWait {
		var ok bool
		observer, ok = s.Minecraft.(minecraft.Observer)
		if !ok {
			return result, ErrUnsupported
		}
		caps, err := s.Minecraft.Capabilities(ctx)
		if err != nil {
			return result, fmt.Errorf("minecraft capabilities: %w", err)
		}
		if !caps.Readiness {
			return result, ErrUnsupported
		}
	}
	err := s.graceful(ctx, opts.StopTimeout, &result)
	if errors.Is(err, errWaitTimeout) && ctx.Err() == nil && opts.Escalation >= WithTerm {
		err = s.signalAndWait(ctx, runtime.Terminate, TermSent, TermTimedOut, opts.TermTimeout, &result)
	}
	if errors.Is(err, errWaitTimeout) && ctx.Err() == nil && opts.Escalation == WithKill {
		err = s.signalAndWait(ctx, runtime.Kill, KillSent, KillTimedOut, opts.KillTimeout, &result)
	}
	if err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	result.Events = append(result.Events, StartRequested)
	if err := s.startProcess(ctx); err != nil {
		return result, err
	}
	result.Events = append(result.Events, ProcessStarted)
	result.State = minecraft.Starting
	result.Exit = nil
	if opts.NoWait {
		return result, nil
	}
	err = s.awaitStartup(ctx, opts.StartTimeout, observer, &result)
	return result, err
}

func describeExit(exit runtime.ExitStatus) string {
	switch {
	case !exit.Known:
		return "exit status unknown"
	case exit.Signal != nil && exit.Signal.Name != "":
		return fmt.Sprintf("terminated by signal %s", exit.Signal.Name)
	case exit.Signal != nil:
		return fmt.Sprintf("terminated by signal %d", exit.Signal.Number)
	default:
		return fmt.Sprintf("exit code %d", exit.Code)
	}
}

func (s Service) signalAndWait(ctx context.Context, signal runtime.Signal, sent, timedOut Event, timeout time.Duration, result *Result) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.Runtime.Signal(ctx, signal); err != nil {
		return fmt.Errorf("send %s: %w", signal, err)
	}
	result.Events = append(result.Events, sent)
	return s.wait(ctx, timeout, result, timedOut)
}

// awaitStartup observes both process exit and Minecraft readiness. No signals or
// retries are issued on startup failure, timeout, observer error, or cancellation.
func (s Service) awaitStartup(ctx context.Context, timeout time.Duration, observer minecraft.Observer, result *Result) error {
	child, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	finish := func(err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if child.Err() == context.DeadlineExceeded {
			result.Events = append(result.Events, StartupTimedOut)
			return fmt.Errorf("startup: %w", ErrTimeout)
		}
		return err
	}
	for {
		if err := child.Err(); err != nil {
			return finish(err)
		}
		state, err := s.Runtime.State(child)
		if err != nil {
			return finish(fmt.Errorf("startup runtime state: %w", err))
		}
		if state == runtime.Stopped {
			result.State = minecraft.Stopped
			result.Events = append(result.Events, StartupExited)
			exit, err := s.Runtime.Wait(child)
			if err != nil {
				return finish(fmt.Errorf("startup exit: %w", err))
			}
			result.Exit = &exit
			return fmt.Errorf("process exited before Minecraft startup completed (%s): %w", describeExit(exit), ErrStartupFailed)
		}
		if state == runtime.Running {
			mcState, err := observer.State(child)
			if err != nil {
				return finish(fmt.Errorf("startup observation: %w", err))
			}
			if err := child.Err(); err != nil {
				return finish(err)
			}
			if mcState == minecraft.Running {
				// Recheck liveness after observation; never promote a known exited process.
				state, err = s.Runtime.State(child)
				if err != nil {
					return finish(fmt.Errorf("startup runtime state: %w", err))
				}
				if err := child.Err(); err != nil {
					return finish(err)
				}
				if state == runtime.Running {
					result.State = minecraft.Running
					result.Events = append(result.Events, StartupCompleted)
					return nil
				}
				if state == runtime.Stopped {
					continue
				}
			}
		}
		select {
		case <-child.Done():
			return finish(child.Err())
		case <-ticker.C:
		}
	}
}
