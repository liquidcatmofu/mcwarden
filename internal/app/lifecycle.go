package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/liquidcatmofu/mcwarden/internal/minecraft"
	"github.com/liquidcatmofu/mcwarden/internal/runtime"
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
	StopTimeout  time.Duration
	TermTimeout  time.Duration
	KillTimeout  time.Duration
	Escalation   Escalation
	AllowSIGKILL bool
}

type Phase string

const (
	StopRequested Phase = "stop_requested"
	StopTimedOut  Phase = "stop_timed_out"
	TermSent      Phase = "term_sent"
	TermTimedOut  Phase = "term_timed_out"
	KillSent      Phase = "kill_sent"
	KillTimedOut  Phase = "kill_timed_out"
	Stopped       Phase = "stopped"
	Started       Phase = "started"
)

// Result records completed transitions, including those preceding an error.
type Result struct {
	Phases []Phase `json:"phases"`
}

type Service struct {
	Runtime   runtime.Runtime
	Minecraft minecraft.Control
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
	result.Phases = append(result.Phases, StopRequested)
	return s.wait(ctx, timeout, result, StopTimedOut)
}

func (s Service) wait(ctx context.Context, timeout time.Duration, result *Result, timedOut Phase) error {
	child, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, err := s.Runtime.Wait(child)
	// Parent cancellation/deadline is never a reason to escalate.
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && child.Err() == context.DeadlineExceeded {
			result.Phases = append(result.Phases, timedOut)
			return fmt.Errorf("%s: %w", timedOut, errors.Join(ErrTimeout, errWaitTimeout))
		}
		return fmt.Errorf("wait for exit: %w", err)
	}
	result.Phases = append(result.Phases, Stopped)
	return nil
}

// Stop never sends OS signals, even on timeout or transport failure.
func (s Service) Stop(ctx context.Context, opts StopOptions) (Result, error) {
	result := Result{Phases: []Phase{}}
	if opts.Timeout <= 0 {
		return result, fmt.Errorf("stop timeout must be positive: %w", ErrInvalid)
	}
	err := s.graceful(ctx, opts.Timeout, &result)
	return result, err
}

// Restart escalates only after its own wait timeout, never after an arbitrary
// error. Start is called only after Wait positively confirms the old process exited.
func (s Service) Restart(ctx context.Context, opts RestartOptions) (Result, error) {
	result := Result{Phases: []Phase{}}
	if opts.StopTimeout <= 0 || opts.Escalation < StopOnly || opts.Escalation > WithKill ||
		(opts.Escalation >= WithTerm && opts.TermTimeout <= 0) ||
		(opts.Escalation == WithKill && (opts.KillTimeout <= 0 || !opts.AllowSIGKILL)) {
		return result, fmt.Errorf("invalid restart timeout or escalation authorization: %w", ErrInvalid)
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
	if err := s.Runtime.Start(ctx); err != nil {
		return result, fmt.Errorf("start: %w", err)
	}
	result.Phases = append(result.Phases, Started)
	return result, nil
}

func (s Service) signalAndWait(ctx context.Context, signal runtime.Signal, sent, timedOut Phase, timeout time.Duration, result *Result) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.Runtime.Signal(ctx, signal); err != nil {
		return fmt.Errorf("send %s: %w", signal, err)
	}
	result.Phases = append(result.Phases, sent)
	return s.wait(ctx, timeout, result, timedOut)
}
