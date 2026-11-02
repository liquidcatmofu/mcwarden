package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/liquidcatmofu/mcwarden/internal/minecraft"
	"github.com/liquidcatmofu/mcwarden/internal/minecraft/rcon"
	"github.com/liquidcatmofu/mcwarden/internal/runtime"
)

var errTransport = errors.New("transport failed")

// A zero wait entry waits for the real context deadline; nil entries exit.
type fake struct {
	calls                                              []string
	state                                              runtime.State
	waits                                              []error
	capsErr, commandErr, signalErr, startErr, stateErr error
	termSignalErr                                      error
	termCancel                                         context.CancelFunc
	noCommand                                          bool
	cancel                                             context.CancelFunc
}

func (f *fake) State(context.Context) (runtime.State, error) {
	f.calls = append(f.calls, "state")
	return f.state, f.stateErr
}
func (f *fake) Start(context.Context) error { f.calls = append(f.calls, "start"); return f.startErr }
func (f *fake) Signal(_ context.Context, sig runtime.Signal) error {
	f.calls = append(f.calls, string(sig))
	if sig == runtime.Terminate && f.termCancel != nil {
		f.termCancel()
	}
	if sig == runtime.Terminate && f.termSignalErr != nil {
		return f.termSignalErr
	}
	return f.signalErr
}

func TestRestartCanceledUnsupportedTermDoesNotKill(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fake{state: runtime.Running, waits: []error{context.DeadlineExceeded}, termSignalErr: ErrUnsupported, termCancel: cancel}
	_, err := (Service{Runtime: f, Minecraft: f}).Restart(ctx, RestartOptions{NoWait: true, StopTimeout: time.Millisecond, TermTimeout: time.Millisecond, KillTimeout: time.Millisecond, Escalation: WithKill, AllowSIGKILL: true})
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(f.calls, []string{"state", "stop", "wait", "term"}) {
		t.Fatalf("error=%v calls=%v", err, f.calls)
	}
}

func TestRestartUnsupportedTerminate(t *testing.T) {
	for _, tt := range []struct {
		name    string
		level   Escalation
		termErr error
		waits   []error
		want    error
		calls   []string
	}{
		{"term unsupported", WithTerm, ErrUnsupported, []error{context.DeadlineExceeded}, ErrUnsupported, []string{"state", "stop", "wait", "term"}},
		{"kill skips unsupported term", WithKill, fmt.Errorf("TERM-only: %w", ErrUnsupported), []error{context.DeadlineExceeded, nil}, nil, []string{"state", "stop", "wait", "term", "kill", "wait", "start"}},
		{"external term failure", WithKill, ErrExternal, []error{context.DeadlineExceeded}, ErrExternal, []string{"state", "stop", "wait", "term"}},
		{"term cancellation", WithKill, context.Canceled, []error{context.DeadlineExceeded}, context.Canceled, []string{"state", "stop", "wait", "term"}},
		{"term deadline", WithKill, context.DeadlineExceeded, []error{context.DeadlineExceeded}, context.DeadlineExceeded, []string{"state", "stop", "wait", "term"}},
		{"unsupported graceful wait", WithKill, nil, []error{ErrUnsupported}, ErrUnsupported, []string{"state", "stop", "wait"}},
		{"unsupported term wait", WithKill, nil, []error{context.DeadlineExceeded, ErrUnsupported}, ErrUnsupported, []string{"state", "stop", "wait", "term", "wait"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fake{state: runtime.Running, waits: tt.waits, termSignalErr: tt.termErr}
			result, err := (Service{Runtime: f, Minecraft: f}).Restart(context.Background(), RestartOptions{NoWait: true, StopTimeout: time.Millisecond, TermTimeout: time.Millisecond, KillTimeout: time.Millisecond, Escalation: tt.level, AllowSIGKILL: true})
			if !errors.Is(err, tt.want) || !reflect.DeepEqual(f.calls, tt.calls) {
				t.Fatalf("error=%v calls=%v want %v %v", err, f.calls, tt.want, tt.calls)
			}
			if tt.termErr != nil {
				for _, event := range result.Events {
					if event == TermSent || event == TermTimedOut {
						t.Fatalf("unsupported/failed TERM must not produce TERM events: %v", result.Events)
					}
				}
			}
		})
	}
}
func (f *fake) Command(_ context.Context, command string) error {
	f.calls = append(f.calls, command)
	return f.commandErr
}
func (f *fake) Capabilities(context.Context) (minecraft.Capabilities, error) {
	return minecraft.Capabilities{Command: !f.noCommand}, f.capsErr
}
func (f *fake) Wait(ctx context.Context) (runtime.ExitStatus, error) {
	f.calls = append(f.calls, "wait")
	if f.cancel != nil {
		f.cancel()
		return runtime.ExitStatus{}, ctx.Err()
	}
	if len(f.waits) == 0 {
		f.state = runtime.Stopped
		return runtime.ExitStatus{Code: 1}, nil
	}
	err := f.waits[0]
	f.waits = f.waits[1:]
	if errors.Is(err, context.DeadlineExceeded) {
		<-ctx.Done()
		err = ctx.Err()
	}
	if err == nil {
		f.state = runtime.Stopped
	}
	return runtime.ExitStatus{Code: 1}, err
}

func TestRestart(t *testing.T) {
	base := RestartOptions{NoWait: true, StopTimeout: time.Millisecond, TermTimeout: time.Millisecond, KillTimeout: time.Millisecond}
	tests := []struct {
		name                            string
		level                           Escalation
		force                           bool
		waits                           []error
		commandErr, signalErr, startErr error
		wantErr                         error
		wantCalls                       []string
		wantEvents                      []Event
	}{
		{name: "graceful nonzero exit", wantCalls: []string{"state", "stop", "wait", "start"}, wantEvents: []Event{StopRequested, Stopped, StartRequested, ProcessStarted}},
		{name: "stop only timeout", waits: []error{context.DeadlineExceeded}, wantErr: ErrTimeout, wantCalls: []string{"state", "stop", "wait"}, wantEvents: []Event{StopRequested, StopTimedOut}},
		{name: "term", level: WithTerm, waits: []error{context.DeadlineExceeded, nil}, wantCalls: []string{"state", "stop", "wait", "term", "wait", "start"}, wantEvents: []Event{StopRequested, StopTimedOut, TermSent, Stopped, StartRequested, ProcessStarted}},
		{name: "term timeout", level: WithTerm, waits: []error{context.DeadlineExceeded, context.DeadlineExceeded}, wantErr: ErrTimeout, wantCalls: []string{"state", "stop", "wait", "term", "wait"}, wantEvents: []Event{StopRequested, StopTimedOut, TermSent, TermTimedOut}},
		{name: "kill", level: WithKill, force: true, waits: []error{context.DeadlineExceeded, context.DeadlineExceeded, nil}, wantCalls: []string{"state", "stop", "wait", "term", "wait", "kill", "wait", "start"}, wantEvents: []Event{StopRequested, StopTimedOut, TermSent, TermTimedOut, KillSent, Stopped, StartRequested, ProcessStarted}},
		{name: "kill timeout", level: WithKill, force: true, waits: []error{context.DeadlineExceeded, context.DeadlineExceeded, context.DeadlineExceeded}, wantErr: ErrTimeout, wantCalls: []string{"state", "stop", "wait", "term", "wait", "kill", "wait"}, wantEvents: []Event{StopRequested, StopTimedOut, TermSent, TermTimedOut, KillSent, KillTimedOut}},
		{name: "kill needs force", level: WithKill, wantErr: ErrInvalid, wantEvents: []Event{}},
		{name: "command error", level: WithKill, force: true, commandErr: errTransport, wantErr: errTransport, wantCalls: []string{"state", "stop"}, wantEvents: []Event{}},
		{name: "command timeout is not wait timeout", level: WithKill, force: true, commandErr: ErrTimeout, wantErr: ErrTimeout, wantCalls: []string{"state", "stop"}, wantEvents: []Event{}},
		{name: "wait error", level: WithKill, force: true, waits: []error{errTransport}, wantErr: errTransport, wantCalls: []string{"state", "stop", "wait"}, wantEvents: []Event{StopRequested}},
		{name: "signal error", level: WithKill, force: true, waits: []error{context.DeadlineExceeded}, signalErr: ErrTimeout, wantErr: ErrTimeout, wantCalls: []string{"state", "stop", "wait", "term"}, wantEvents: []Event{StopRequested, StopTimedOut}},
		{name: "start error", startErr: errTransport, wantErr: errTransport, wantCalls: []string{"state", "stop", "wait", "start"}, wantEvents: []Event{StopRequested, Stopped, StartRequested}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fake{state: runtime.Running, waits: tt.waits, commandErr: tt.commandErr, signalErr: tt.signalErr, startErr: tt.startErr}
			opts := base
			opts.Escalation = tt.level
			opts.AllowSIGKILL = tt.force
			result, err := (Service{Runtime: f, Minecraft: f}).Restart(context.Background(), opts)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(f.calls, tt.wantCalls) {
				t.Errorf("calls = %v, want %v", f.calls, tt.wantCalls)
			}
			if !reflect.DeepEqual(result.Events, tt.wantEvents) {
				t.Errorf("phases = %v, want %v", result.Events, tt.wantEvents)
			}
		})
	}
}

func TestStopNeverEscalates(t *testing.T) {
	f := &fake{state: runtime.Running, waits: []error{context.DeadlineExceeded}}
	_, err := (Service{Runtime: f, Minecraft: f}).Stop(context.Background(), StopOptions{Timeout: time.Millisecond})
	if !errors.Is(err, ErrTimeout) || !reflect.DeepEqual(f.calls, []string{"state", "stop", "wait"}) {
		t.Fatalf("calls=%v error=%v", f.calls, err)
	}
}

func TestRestartCancellationDoesNotEscalate(t *testing.T) {
	for _, parentDeadline := range []bool{false, true} {
		t.Run(fmt.Sprint(parentDeadline), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f := &fake{state: runtime.Running, cancel: cancel}
			want := context.Canceled
			if parentDeadline {
				var deadlineCancel context.CancelFunc
				ctx, deadlineCancel = context.WithTimeout(context.Background(), time.Millisecond)
				defer deadlineCancel()
				f.cancel = nil
				f.waits = []error{context.DeadlineExceeded}
				want = context.DeadlineExceeded
			}
			_, err := (Service{Runtime: f, Minecraft: f}).Restart(ctx, RestartOptions{NoWait: true, StopTimeout: time.Second, TermTimeout: time.Second, KillTimeout: time.Second, Escalation: WithKill, AllowSIGKILL: true})
			if !errors.Is(err, want) {
				t.Fatalf("error = %v", err)
			}
			for _, call := range f.calls {
				if call == "term" || call == "kill" || call == "start" {
					t.Fatalf("unexpected action: %v", f.calls)
				}
			}
		})
	}
}

func TestPreflightNoSideEffects(t *testing.T) {
	for _, tt := range []struct {
		name string
		f    fake
		opts StopOptions
		want error
	}{
		{"stopped", fake{state: runtime.Stopped}, StopOptions{time.Second}, ErrNotRunning},
		{"unknown", fake{state: runtime.Unknown}, StopOptions{time.Second}, ErrStateUnknown},
		{"unsupported", fake{state: runtime.Running, noCommand: true}, StopOptions{time.Second}, ErrUnsupported},
		{"invalid timeout", fake{state: runtime.Running}, StopOptions{}, ErrInvalid},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := (Service{Runtime: &tt.f, Minecraft: &tt.f}).Stop(context.Background(), tt.opts)
			if err == nil || (tt.want != nil && !errors.Is(err, tt.want)) {
				t.Fatalf("error=%v", err)
			}
			for _, call := range tt.f.calls {
				if call != "state" {
					t.Fatalf("unexpected action: %v", tt.f.calls)
				}
			}
		})
	}
}

func TestStopSendsMinecraftStopCommand(t *testing.T) {
	f := &fake{state: runtime.Running}
	result, err := (Service{Runtime: f, Minecraft: f}).Stop(context.Background(), StopOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !reflect.DeepEqual(f.calls, []string{"state", "stop", "wait"}) {
		t.Fatalf("calls=%v", f.calls)
	}
	if !reflect.DeepEqual(result.Events, []Event{StopRequested, Stopped}) {
		t.Fatalf("events=%v", result.Events)
	}
}

// A failed RCON command is a transport failure, not a wait timeout, so it must
// never cause TERM/KILL escalation or a restart.
func TestRCONCommandFailureDoesNotEscalate(t *testing.T) {
	f := &fake{state: runtime.Running, commandErr: errTransport}
	_, err := (Service{Runtime: f, Minecraft: f}).Restart(context.Background(), RestartOptions{
		NoWait: true, StopTimeout: time.Second, TermTimeout: time.Second, KillTimeout: time.Second,
		Escalation: WithKill, AllowSIGKILL: true,
	})
	if !errors.Is(err, errTransport) {
		t.Fatalf("err=%v", err)
	}
	for _, call := range f.calls {
		if call == "term" || call == "kill" || call == "start" {
			t.Fatalf("unexpected escalation or start: %v", f.calls)
		}
	}
}

// Missing or empty RCON credentials are command transport errors, not wait
// timeouts, so they must not trigger signal escalation either.
func TestRCONCredentialFailureDoesNotEscalate(t *testing.T) {
	f := &fake{state: runtime.Running, commandErr: rcon.ErrEmptyPassword}
	_, err := (Service{Runtime: f, Minecraft: f}).Restart(context.Background(), RestartOptions{
		NoWait: true, StopTimeout: time.Second, TermTimeout: time.Second, KillTimeout: time.Second,
		Escalation: WithKill, AllowSIGKILL: true,
	})
	if !errors.Is(err, rcon.ErrEmptyPassword) {
		t.Fatalf("err=%v", err)
	}
	for _, call := range f.calls {
		if call == "term" || call == "kill" || call == "start" {
			t.Fatalf("unexpected escalation or start: %v", f.calls)
		}
	}
}

func TestRestartNoWaitStartsAfterGracefulExit(t *testing.T) {
	f := &fake{state: runtime.Running}
	result, err := (Service{Runtime: f, Minecraft: f}).Restart(context.Background(), RestartOptions{
		NoWait: true, StopTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if !reflect.DeepEqual(f.calls, []string{"state", "stop", "wait", "start"}) {
		t.Fatalf("calls=%v", f.calls)
	}
	if !reflect.DeepEqual(result.Events, []Event{StopRequested, Stopped, StartRequested, ProcessStarted}) {
		t.Fatalf("events=%v", result.Events)
	}
}

func TestStatusPartialFailure(t *testing.T) {
	f := &fake{state: runtime.Running, capsErr: errTransport}
	status, err := (Service{Runtime: f, Minecraft: f}).Status(context.Background())
	if err != nil || status.Runtime.State != runtime.Running || status.Minecraft.Capabilities != nil || status.Minecraft.Error == "" {
		t.Fatalf("status=%+v error=%v", status, err)
	}
	f.stateErr = errTransport
	if _, err := (Service{Runtime: f}).Status(context.Background()); !errors.Is(err, errTransport) {
		t.Fatalf("error=%v", err)
	}
}
