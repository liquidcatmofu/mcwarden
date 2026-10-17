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
 launched bool
 states []runtime.State
}

func (f *startupRuntime) Start(ctx context.Context) error {
 err := f.fake.Start(ctx)
 f.launched = err == nil
 return err
}
func (f *startupRuntime) State(ctx context.Context) (runtime.State, error) {
 if !f.launched { return f.fake.State(ctx) }
 f.calls = append(f.calls, "startup_state")
 if len(f.states) > 0 {
  state := f.states[0]
  f.states = f.states[1:]
  return state, nil
 }
 return runtime.Running, nil
}

type startupControl struct {
 runtime *startupRuntime
 observe func(context.Context) (minecraft.State, error)
 noReadiness bool
 observations int
}
func (f *startupControl) Command(ctx context.Context, command string) error {
 return f.runtime.Command(ctx, command)
}
func (f *startupControl) Capabilities(context.Context) (minecraft.Capabilities, error) {
 return minecraft.Capabilities{Command: true, Readiness: !f.noReadiness}, nil
}
func (f *startupControl) State(ctx context.Context) (minecraft.State, error) {
 f.observations++
 if f.observe != nil { return f.observe(ctx) }
 return minecraft.Running, nil
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
      if name == "unknown then ready" { return minecraft.Unknown, nil }
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
   result, err := (Service{Runtime: rt, Minecraft: mc}).Restart(ctx, opts)
   if !errors.Is(err, wantErr) || result.State != wantState {
    t.Fatalf("result=%+v error=%v want state=%s error=%v", result, err, wantState, wantErr)
   }
   if name == "unsupported" {
    if len(rt.calls) != 0 { t.Fatalf("preflight caused side effects: %v", rt.calls) }
    return
   }
   if got := result.Events[len(result.Events)-1]; got != wantEvent { t.Fatalf("events=%v", result.Events) }
   if wantState == minecraft.Stopped {
    if result.Exit == nil || result.Exit.Code != 1 { t.Fatalf("exit=%+v", result.Exit) }
   } else if result.Exit != nil { t.Fatalf("old exit leaked into new process: %+v", result.Exit) }
   if name == "no wait" && mc.observations != 0 { t.Fatal("no-wait observed readiness") }
   for _, call := range rt.calls {
    if call == "term" || call == "kill" { t.Fatalf("unexpected signal: %v", rt.calls) }
   }
  })
 }
}

func TestRestartReadinessPreflight(t *testing.T) {
 f := &fake{state: runtime.Running}
 for _, timeout := range []time.Duration{0, time.Second} {
  _, err := (Service{Runtime: f, Minecraft: f}).Restart(context.Background(), RestartOptions{StopTimeout: time.Second, StartTimeout: timeout})
  want := ErrUnsupported
  if timeout == 0 { want = ErrInvalid }
  if !errors.Is(err, want) || len(f.calls) != 0 { t.Fatalf("error=%v calls=%v", err, f.calls) }
 }
}

func TestStatusMinecraftLifecycle(t *testing.T) {
 rt := &startupRuntime{fake: fake{state: runtime.Running}}
 mc := &startupControl{runtime: rt, observe: func(context.Context) (minecraft.State, error) { return minecraft.Starting, nil }}
 status, err := (Service{Runtime: rt, Minecraft: mc}).Status(context.Background())
 if err != nil || status.Minecraft.State != minecraft.Starting { t.Fatalf("status=%+v err=%v", status, err) }
 mc.observe = func(context.Context) (minecraft.State, error) { return minecraft.Unknown, errTransport }
 status, err = (Service{Runtime: rt, Minecraft: mc}).Status(context.Background())
 if err != nil || status.Minecraft.State != minecraft.Unknown || status.Minecraft.Error == "" { t.Fatalf("status=%+v err=%v", status, err) }
 rt.state = runtime.Stopped
 before := mc.observations
 status, err = (Service{Runtime: rt, Minecraft: mc}).Status(context.Background())
 if err != nil || status.Minecraft.State != minecraft.Stopped || before != mc.observations { t.Fatalf("status=%+v err=%v", status, err) }
 f := &fake{state: runtime.Running}
 status, err = (Service{Runtime: f, Minecraft: f}).Status(context.Background())
 if err != nil || status.Minecraft.State != minecraft.Unknown { t.Fatalf("status=%+v err=%v", status, err) }
}

func TestStopRecordsStateAndEvents(t *testing.T) {
 f := &fake{state: runtime.Running, waits: []error{context.DeadlineExceeded}}
 result, err := (Service{Runtime: f, Minecraft: f}).Stop(context.Background(), StopOptions{Timeout: time.Millisecond})
 if !errors.Is(err, ErrTimeout) || result.State != minecraft.Stopping || !reflect.DeepEqual(result.Events, []Event{StopRequested, StopTimedOut}) {
  t.Fatalf("result=%+v err=%v", result, err)
 }
}
