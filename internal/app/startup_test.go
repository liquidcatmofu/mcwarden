package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/liquidcatmofu/mcwarden/internal/minecraft"
	"github.com/liquidcatmofu/mcwarden/internal/runtime"
)

type startupRuntime struct {
	fake
	launched       bool
	states         []runtime.State
	identity       runtime.Identity
	hasID          bool
	generation     string
	nextGeneration string
	noGeneration   bool
	order          *[]string
}

func (f *startupRuntime) Identity() (runtime.Identity, bool) { return f.identity, f.hasID }

func (f *startupRuntime) Generation(context.Context) (string, bool, error) {
	if f.noGeneration {
		return "", false, nil
	}
	if f.generation == "" {
		return "generation", true, nil
	}
	return f.generation, true, nil
}

func (f *startupRuntime) Start(ctx context.Context) error {
	if f.order != nil {
		*f.order = append(*f.order, "start")
	}
	err := f.fake.Start(ctx)
	f.launched = err == nil
	if err == nil && f.nextGeneration != "" {
		f.generation = f.nextGeneration
	}
	return err
}
func (f *startupRuntime) State(ctx context.Context) (runtime.State, error) {
	if !f.launched {
		return f.fake.State(ctx)
	}
	f.calls = append(f.calls, "startup_state")
	if len(f.states) > 0 {
		state := f.states[0]
		f.states = f.states[1:]
		return state, nil
	}
	return runtime.Running, nil
}

type startupControl struct {
	runtime      *startupRuntime
	observe      func(context.Context) (minecraft.State, error)
	noReadiness  bool
	observations int
	prepares     int
	bindKeys     []string
	prepareErr   error
	bindErr      error
	order        *[]string
}

func (f *startupControl) Command(ctx context.Context, command string) error {
	return f.runtime.Command(ctx, command)
}
func (f *startupControl) Capabilities(context.Context) (minecraft.Capabilities, error) {
	return minecraft.Capabilities{Command: true, Readiness: !f.noReadiness}, nil
}
func (f *startupControl) State(ctx context.Context, _ string) (minecraft.State, error) {
	f.observations++
	if f.observe != nil {
		return f.observe(ctx)
	}
	return minecraft.Running, nil
}
func (f *startupControl) Prepare(context.Context) error {
	f.prepares++
	if f.order != nil {
		*f.order = append(*f.order, "prepare")
	}
	return f.prepareErr
}
func (f *startupControl) Bind(_ context.Context, generation string) error {
	f.bindKeys = append(f.bindKeys, generation)
	if f.order != nil {
		*f.order = append(*f.order, "bind")
	}
	return f.bindErr
}

func TestRestartStartup(t *testing.T) {
	for _, name := range []string{"ready", "starting then ready", "unknown then ready", "timeout", "cancel", "observer error", "exit", "exit after ready observation", "no wait", "unsupported"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rt := &startupRuntime{fake: fake{state: runtime.Running}}
			mc := &startupControl{runtime: rt}
			opts := RestartOptions{StopTimeout: time.Second, StartTimeout: time.Second}
			wantState := minecraft.Running
			var wantErr error
			wantEvent := StartupCompleted
			switch name {
			case "starting then ready", "unknown then ready":
				mc.observe = func(context.Context) (minecraft.State, error) {
					if mc.observations == 1 {
						if name == "unknown then ready" {
							return minecraft.Unknown, nil
						}
						return minecraft.Starting, nil
					}
					return minecraft.Running, nil
				}
			case "timeout":
				opts.StartTimeout = time.Millisecond
				mc.observe = func(ctx context.Context) (minecraft.State, error) { <-ctx.Done(); return minecraft.Unknown, ctx.Err() }
				wantState, wantErr, wantEvent = minecraft.Starting, ErrTimeout, StartupTimedOut
			case "cancel":
				mc.observe = func(ctx context.Context) (minecraft.State, error) { cancel(); return minecraft.Running, nil }
				wantState, wantErr, wantEvent = minecraft.Starting, context.Canceled, ProcessStarted
			case "observer error":
				mc.observe = func(context.Context) (minecraft.State, error) { return minecraft.Unknown, errTransport }
				wantState, wantErr, wantEvent = minecraft.Starting, errTransport, ProcessStarted
			case "exit":
				rt.states = []runtime.State{runtime.Stopped}
				wantState, wantErr, wantEvent = minecraft.Stopped, ErrStartupFailed, StartupExited
			case "exit after ready observation":
				rt.states = []runtime.State{runtime.Running, runtime.Stopped, runtime.Stopped}
				wantState, wantErr, wantEvent = minecraft.Stopped, ErrStartupFailed, StartupExited
			case "no wait":
				opts.NoWait = true
				mc.noReadiness = true
				wantState, wantEvent = minecraft.Starting, ProcessStarted
			case "unsupported":
				mc.noReadiness = true
				wantState, wantErr = minecraft.Unknown, ErrUnsupported
			}
			service := Service{Runtime: rt, Minecraft: mc}
			if name != "no wait" && name != "unsupported" {
				service.Observer = mc
			}
			result, err := service.Restart(ctx, opts)
			if !errors.Is(err, wantErr) || result.State != wantState {
				t.Fatalf("result=%+v error=%v want state=%s error=%v", result, err, wantState, wantErr)
			}
			if name == "unsupported" {
				if len(rt.calls) != 0 || mc.prepares != 0 {
					t.Fatalf("preflight caused side effects: %v prepares=%d", rt.calls, mc.prepares)
				}
				return
			}
			if name != "no wait" {
				if mc.prepares != 1 || len(mc.bindKeys) != 1 || mc.bindKeys[0] != "generation" {
					t.Fatalf("prepares=%d bind=%v", mc.prepares, mc.bindKeys)
				}
			}
			if got := result.Events[len(result.Events)-1]; got != wantEvent {
				t.Fatalf("events=%v", result.Events)
			}
			if wantState == minecraft.Stopped {
				if result.Exit == nil || result.Exit.Code != 1 {
					t.Fatalf("exit=%+v", result.Exit)
				}
			} else if result.Exit != nil {
				t.Fatalf("old exit leaked into new process: %+v", result.Exit)
			}
			if name == "no wait" && mc.observations != 0 {
				t.Fatal("no-wait observed readiness")
			}
			for _, call := range rt.calls {
				if call == "term" || call == "kill" {
					t.Fatalf("unexpected signal: %v", rt.calls)
				}
			}
		})
	}
}

func TestRestartReadinessPreflight(t *testing.T) {
	f := &fake{state: runtime.Running}
	for _, timeout := range []time.Duration{0, time.Second} {
		_, err := (Service{Runtime: f, Minecraft: f}).Restart(context.Background(), RestartOptions{StopTimeout: time.Second, StartTimeout: timeout})
		want := ErrUnsupported
		if timeout == 0 {
			want = ErrInvalid
		}
		if !errors.Is(err, want) || len(f.calls) != 0 {
			t.Fatalf("error=%v calls=%v", err, f.calls)
		}
	}
}

func TestStatusMinecraftLifecycle(t *testing.T) {
	rt := &startupRuntime{fake: fake{state: runtime.Running}}
	mc := &startupControl{runtime: rt, observe: func(context.Context) (minecraft.State, error) { return minecraft.Starting, nil }}
	service := Service{Runtime: rt, Minecraft: mc, Observer: mc}
	status, err := service.Status(context.Background())
	if err != nil || status.Minecraft.State != minecraft.Starting {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	mc.observe = func(context.Context) (minecraft.State, error) { return minecraft.Unknown, errTransport }
	status, err = service.Status(context.Background())
	if err != nil || status.Minecraft.State != minecraft.Unknown || status.Minecraft.Error == "" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	rt.state = runtime.Stopped
	before := mc.observations
	status, err = service.Status(context.Background())
	if err != nil || status.Minecraft.State != minecraft.Stopped || before != mc.observations {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	f := &fake{state: runtime.Running}
	status, err = (Service{Runtime: f, Minecraft: f}).Status(context.Background())
	if err != nil || status.Minecraft.State != minecraft.Unknown {
		t.Fatalf("status=%+v err=%v", status, err)
	}
}

func TestStopRecordsStateAndEvents(t *testing.T) {
	f := &fake{state: runtime.Running, waits: []error{context.DeadlineExceeded}}
	result, err := (Service{Runtime: f, Minecraft: f}).Stop(context.Background(), StopOptions{Timeout: time.Millisecond})
	if !errors.Is(err, ErrTimeout) || result.State != minecraft.Stopping || !reflect.DeepEqual(result.Events, []Event{StopRequested, StopTimedOut}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

// TestStartupExitCleansState verifies that a process which exits during startup
// does not leave stale identity behind to block the next start.
func TestStartupExitCleansState(t *testing.T) {
	rt := &startupRuntime{
		fake:     fake{state: runtime.Stopped},
		states:   []runtime.State{runtime.Stopped},
		hasID:    true,
		identity: runtime.Identity{PID: 1 << 30, PGID: 1 << 30, StartTime: 1, Owned: true},
	}
	service := stateService(t, rt, "rail")
	mc := &startupControl{runtime: rt}
	service.Minecraft = mc
	service.Observer = mc

	if _, err := service.Start(context.Background(), StartOptions{StartTimeout: time.Second}); !errors.Is(err, ErrStartupFailed) {
		t.Fatalf("err=%v", err)
	}
	store, err := service.StateStore()
	if err != nil {
		t.Fatal(err)
	}
	requireAbsent(t, store)
}

func TestStartReadinessEvidence(t *testing.T) {
	t.Run("normal start waits for running", func(t *testing.T) {
		var order []string
		rt := &startupRuntime{fake: fake{state: runtime.Stopped}, order: &order}
		mc := &startupControl{runtime: rt, order: &order}
		result, err := (Service{Runtime: rt, Minecraft: mc, Observer: mc}).Start(context.Background(), StartOptions{StartTimeout: time.Second})
		if err != nil || result.State != minecraft.Running {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if !reflect.DeepEqual(order, []string{"prepare", "start", "bind"}) {
			t.Fatalf("order=%v", order)
		}
		if len(mc.bindKeys) != 1 || mc.bindKeys[0] != "generation" {
			t.Fatalf("bind=%v", mc.bindKeys)
		}
		if mc.observations == 0 {
			t.Fatal("normal start did not observe readiness")
		}
	})

	t.Run("no-wait prepares and binds", func(t *testing.T) {
		var order []string
		rt := &startupRuntime{fake: fake{state: runtime.Stopped}, order: &order}
		mc := &startupControl{runtime: rt, order: &order}
		result, err := (Service{Runtime: rt, Minecraft: mc, Observer: mc}).Start(context.Background(), StartOptions{NoWait: true})
		if err != nil || result.State != minecraft.Starting {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if !reflect.DeepEqual(order, []string{"prepare", "start", "bind"}) {
			t.Fatalf("order=%v", order)
		}
		if mc.observations != 0 {
			t.Fatal("no-wait observed readiness")
		}
		if len(mc.bindKeys) != 1 || mc.bindKeys[0] != "generation" {
			t.Fatalf("bind=%v", mc.bindKeys)
		}
	})

	t.Run("prepare failure prevents start", func(t *testing.T) {
		rt := &startupRuntime{fake: fake{state: runtime.Stopped}}
		mc := &startupControl{runtime: rt, prepareErr: errTransport}
		_, err := (Service{Runtime: rt, Minecraft: mc, Observer: mc}).Start(context.Background(), StartOptions{NoWait: true})
		if !errors.Is(err, errTransport) {
			t.Fatalf("err=%v", err)
		}
		if len(rt.calls) != 0 || len(mc.bindKeys) != 0 {
			t.Fatalf("calls=%v bind=%v", rt.calls, mc.bindKeys)
		}
	})

	t.Run("bind failure does not signal", func(t *testing.T) {
		rt := &startupRuntime{fake: fake{state: runtime.Stopped}}
		mc := &startupControl{runtime: rt, bindErr: errTransport}
		result, err := (Service{Runtime: rt, Minecraft: mc, Observer: mc}).Start(context.Background(), StartOptions{NoWait: true})
		if !errors.Is(err, errTransport) {
			t.Fatalf("err=%v", err)
		}
		if result.State != minecraft.Starting || len(result.Events) == 0 || result.Events[len(result.Events)-1] != ProcessStarted {
			t.Fatalf("result=%+v", result)
		}
		for _, call := range rt.calls {
			if call == "term" || call == "kill" {
				t.Fatalf("bind failure escalated: %v", rt.calls)
			}
		}
	})

	t.Run("missing generation does not signal", func(t *testing.T) {
		rt := &startupRuntime{fake: fake{state: runtime.Stopped}, noGeneration: true}
		mc := &startupControl{runtime: rt}
		result, err := (Service{Runtime: rt, Minecraft: mc, Observer: mc}).Start(context.Background(), StartOptions{NoWait: true})
		if !errors.Is(err, ErrStateUnknown) {
			t.Fatalf("err=%v", err)
		}
		if result.State != minecraft.Starting || len(mc.bindKeys) != 0 {
			t.Fatalf("result=%+v bind=%v", result, mc.bindKeys)
		}
		for _, call := range rt.calls {
			if call == "term" || call == "kill" {
				t.Fatalf("missing generation escalated: %v", rt.calls)
			}
		}
	})

	t.Run("no observer keeps no-wait behavior", func(t *testing.T) {
		rt := &startupRuntime{fake: fake{state: runtime.Stopped}}
		mc := &startupControl{runtime: rt}
		result, err := (Service{Runtime: rt, Minecraft: mc}).Start(context.Background(), StartOptions{NoWait: true})
		if err != nil || result.State != minecraft.Starting {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if mc.prepares != 0 || len(mc.bindKeys) != 0 {
			t.Fatalf("unconfigured observer was used: prepares=%d bind=%v", mc.prepares, mc.bindKeys)
		}
	})
}

func TestRestartBindsNewGeneration(t *testing.T) {
	rt := &startupRuntime{fake: fake{state: runtime.Running}, generation: "old", nextGeneration: "new"}
	mc := &startupControl{runtime: rt}
	result, err := (Service{Runtime: rt, Minecraft: mc, Observer: mc}).Restart(context.Background(), RestartOptions{NoWait: true, StopTimeout: time.Second})
	if err != nil || result.State != minecraft.Starting {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if !reflect.DeepEqual(mc.bindKeys, []string{"new"}) {
		t.Fatalf("bind=%v", mc.bindKeys)
	}
}

func TestStatusReadinessComposition(t *testing.T) {
	rt := &startupRuntime{fake: fake{state: runtime.Running}}
	mc := &startupControl{runtime: rt, observe: func(context.Context) (minecraft.State, error) { return minecraft.Starting, nil }}
	control := &fake{state: runtime.Running}
	service := Service{Runtime: rt, Minecraft: control, Observer: mc}

	status, err := service.Status(context.Background())
	if err != nil || status.Minecraft.State != minecraft.Starting {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	if status.Minecraft.Capabilities == nil || !status.Minecraft.Capabilities.Command || !status.Minecraft.Capabilities.Readiness {
		t.Fatalf("capabilities=%+v", status.Minecraft.Capabilities)
	}

	mc.observe = func(context.Context) (minecraft.State, error) { return minecraft.Running, nil }
	status, err = service.Status(context.Background())
	if err != nil || status.Minecraft.State != minecraft.Running {
		t.Fatalf("status=%+v err=%v", status, err)
	}

	mc.observe = func(context.Context) (minecraft.State, error) { return minecraft.Unknown, nil }
	status, err = service.Status(context.Background())
	if err != nil || status.Minecraft.State != minecraft.Unknown {
		t.Fatalf("status=%+v err=%v", status, err)
	}

	before := mc.observations
	rt.state = runtime.Stopped
	status, err = service.Status(context.Background())
	if err != nil || status.Minecraft.State != minecraft.Stopped || before != mc.observations {
		t.Fatalf("status=%+v err=%v observations=%d", status, err, mc.observations)
	}
}

func TestStatusCapabilitiesAreIndependent(t *testing.T) {
	rt := &startupRuntime{fake: fake{state: runtime.Running}}
	observer := &startupControl{runtime: rt, observe: func(context.Context) (minecraft.State, error) { return minecraft.Running, nil }}

	status, err := (Service{Runtime: rt, Observer: observer}).Status(context.Background())
	if err != nil || status.Minecraft.State != minecraft.Running {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	if status.Minecraft.Capabilities == nil || status.Minecraft.Capabilities.Command || !status.Minecraft.Capabilities.Readiness {
		t.Fatalf("capabilities=%+v", status.Minecraft.Capabilities)
	}

	status, err = (Service{Runtime: rt, Minecraft: &fake{state: runtime.Running}}).Status(context.Background())
	if err != nil || status.Minecraft.State != minecraft.Unknown {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	if status.Minecraft.Capabilities == nil || !status.Minecraft.Capabilities.Command || status.Minecraft.Capabilities.Readiness {
		t.Fatalf("capabilities=%+v", status.Minecraft.Capabilities)
	}
}
