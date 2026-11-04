package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/liquidcatmofu/mcwarden/internal/app"
	"github.com/liquidcatmofu/mcwarden/internal/minecraft"
	"github.com/liquidcatmofu/mcwarden/internal/runtime"
	"github.com/liquidcatmofu/mcwarden/internal/state"
)

func TestRunningGenerationIsObserved(t *testing.T) {
	rt := &fakeRuntime{
		state: runtime.Running, generation: "gen-1", generationOK: true,
		waitStarted: make(chan struct{}, 1),
	}
	obs := &fakeObserver{fn: func(int, string) (minecraft.State, error) {
		return minecraft.Running, nil
	}}
	loader := &scriptLoader{steps: []loaderStep{{service: testService(rt, obs)}}}
	clock := newFakeClock()
	buf := newEventBuffer()
	ctx, cancel := context.WithCancel(context.Background())
	done := runAgent(t, ctx, loader, clock, buf, true)
	defer cancel()

	buf.waitFor(t, 2)
	select {
	case <-rt.waitStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("agent did not start waiting on the generation")
	}
	cancel()
	waitRun(t, done)

	events := decodeEvents(t, buf.all())
	if got := types(events); !equalStrings(got, []string{EventAgentStarted, EventGenerationObserved, EventAgentStopped}) {
		t.Fatalf("types=%v", got)
	}
	observed := events[1]
	if observed.Server != "rail" || observed.Generation != "gen-1" ||
		observed.Runtime != runtime.Running || observed.Minecraft != minecraft.Running {
		t.Fatalf("observed=%+v", observed)
	}
	if rt.waitCalls() != 1 {
		t.Fatalf("wait calls=%d", rt.waitCalls())
	}
	if signals := rt.signalCalls(); len(signals) != 0 {
		t.Fatalf("agent sent signals: %v", signals)
	}
}

func TestReadinessChangeIsEmittedOnceAndRepeatedStatesAreQuiet(t *testing.T) {
	obs := &fakeObserver{called: make(chan int, 16), fn: func(call int, _ string) (minecraft.State, error) {
		if call == 1 {
			return minecraft.Starting, nil
		}
		return minecraft.Running, nil
	}}
	rt := &fakeRuntime{state: runtime.Running, generation: "gen-1", generationOK: true}
	loader := &scriptLoader{steps: []loaderStep{{service: testService(rt, obs)}}}
	clock := newFakeClock()
	buf := newEventBuffer()
	ctx, cancel := context.WithCancel(context.Background())
	done := runAgent(t, ctx, loader, clock, buf, true)
	defer cancel()

	buf.waitFor(t, 2) // agent_started, generation_observed (starting)
	ticker := clock.next(t)
	ticker.Tick()
	obs.waitForCall(t, 2)
	buf.waitFor(t, 3) // minecraft_state_changed running

	ticker.Tick()
	obs.waitForCall(t, 3)
	ticker.Tick()
	obs.waitForCall(t, 4)

	cancel()
	waitRun(t, done)

	events := decodeEvents(t, buf.all())
	changed := countType(events, EventMinecraftStateChanged)
	if changed != 1 {
		t.Fatalf("minecraft_state_changed count=%d events=%v", changed, types(events))
	}
	if events[1].Minecraft != minecraft.Starting {
		t.Fatalf("initial minecraft=%s", events[1].Minecraft)
	}
	if !equalStrings(types(events), []string{
		EventAgentStarted, EventGenerationObserved, EventMinecraftStateChanged, EventAgentStopped,
	}) {
		t.Fatalf("types=%v", types(events))
	}
}

func TestGenerationExitReloadsAndDiscoversReplacement(t *testing.T) {
	oldRuntime := &fakeRuntime{
		state: runtime.Running, generation: "gen-1", generationOK: true,
		waitFn: func(context.Context) (runtime.ExitStatus, error) {
			return runtime.ExitStatus{Known: true, Code: 1}, nil
		},
	}
	newRuntime := &fakeRuntime{
		state: runtime.Running, generation: "gen-2", generationOK: true,
		waitStarted: make(chan struct{}, 1),
	}
	loader := &scriptLoader{steps: []loaderStep{
		{service: testService(oldRuntime, nil)},
		{service: testService(newRuntime, nil)},
	}}
	clock := newFakeClock()
	buf := newEventBuffer()
	ctx, cancel := context.WithCancel(context.Background())
	done := runAgent(t, ctx, loader, clock, buf, true)
	defer cancel()

	buf.waitFor(t, 4) // two agent-visible generation observations
	select {
	case <-newRuntime.waitStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement generation was not waited on")
	}
	cancel()
	waitRun(t, done)

	events := decodeEvents(t, buf.all())
	if !equalStrings(types(events), []string{
		EventAgentStarted,
		EventGenerationObserved,
		EventGenerationExited,
		EventGenerationObserved,
		EventAgentStopped,
	}) {
		t.Fatalf("types=%v", types(events))
	}
	if events[1].Generation != "gen-1" || events[3].Generation != "gen-2" {
		t.Fatalf("generations=%q %q", events[1].Generation, events[3].Generation)
	}
	exit := events[2].Exit
	if exit == nil || !exit.Known || exit.Code != 1 {
		t.Fatalf("exit=%+v", exit)
	}
	if loader.callCount() < 2 {
		t.Fatalf("loader calls=%d", loader.callCount())
	}
	if oldRuntime.waitCalls() != 1 || newRuntime.waitCalls() != 1 {
		t.Fatalf("wait calls old=%d new=%d", oldRuntime.waitCalls(), newRuntime.waitCalls())
	}
}

func TestStoppedServerIsReloadedAfterInterval(t *testing.T) {
	stopped := &fakeRuntime{state: runtime.Stopped}
	running := &fakeRuntime{state: runtime.Running, generation: "gen-1", generationOK: true}
	loader := &scriptLoader{steps: []loaderStep{
		{service: testService(stopped, nil)},
		{service: testService(running, nil)},
	}}
	clock := newFakeClock()
	buf := newEventBuffer()
	ctx, cancel := context.WithCancel(context.Background())
	done := runAgent(t, ctx, loader, clock, buf, true)
	defer cancel()

	buf.waitFor(t, 2) // agent_started, snapshot stopped
	ticker := clock.next(t)
	ticker.Tick()
	buf.waitFor(t, 3) // generation_observed

	cancel()
	waitRun(t, done)

	events := decodeEvents(t, buf.all())
	if !equalStrings(types(events), []string{
		EventAgentStarted, EventSnapshot, EventGenerationObserved, EventAgentStopped,
	}) {
		t.Fatalf("types=%v", types(events))
	}
	if events[1].Runtime != runtime.Stopped || events[1].Minecraft != minecraft.Stopped {
		t.Fatalf("snapshot=%+v", events[1])
	}
	if stopped.waitCalls() != 0 {
		t.Fatalf("Wait called while stopped: %d", stopped.waitCalls())
	}
	if loader.callCount() < 2 {
		t.Fatalf("loader calls=%d", loader.callCount())
	}
}

func TestObserverErrorDoesNotStopRuntimeWaitAndRecovers(t *testing.T) {
	obsErr := errors.New("read latest.log")
	obs := &fakeObserver{called: make(chan int, 16), fn: func(call int, _ string) (minecraft.State, error) {
		switch call {
		case 1:
			return minecraft.Starting, nil
		case 2:
			return minecraft.Unknown, obsErr
		default:
			return minecraft.Running, nil
		}
	}}
	rt := &fakeRuntime{
		state: runtime.Running, generation: "gen-1", generationOK: true,
		waitStarted: make(chan struct{}, 1),
	}
	loader := &scriptLoader{steps: []loaderStep{{service: testService(rt, obs)}}}
	clock := newFakeClock()
	buf := newEventBuffer()
	ctx, cancel := context.WithCancel(context.Background())
	done := runAgent(t, ctx, loader, clock, buf, true)
	defer cancel()

	buf.waitFor(t, 2)
	ticker := clock.next(t)
	select {
	case <-rt.waitStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("agent did not start waiting on the generation")
	}
	ticker.Tick()
	obs.waitForCall(t, 2)
	buf.waitFor(t, 3) // observation_error

	if rt.waitCalls() != 1 {
		t.Fatalf("wait calls after observer error=%d", rt.waitCalls())
	}

	ticker.Tick()
	obs.waitForCall(t, 3)
	buf.waitFor(t, 5) // observation_recovered, minecraft_state_changed

	cancel()
	waitRun(t, done)

	events := decodeEvents(t, buf.all())
	if got := countType(events, EventObservationError); got != 1 {
		t.Fatalf("observation_error count=%d", got)
	}
	if got := countType(events, EventObservationRecovered); got != 1 {
		t.Fatalf("observation_recovered count=%d", got)
	}
	if got := countType(events, EventGenerationExited); got != 0 {
		t.Fatalf("unexpected generation_exited count=%d", got)
	}
	if rt.waitCalls() != 1 {
		t.Fatalf("wait was restarted: %d", rt.waitCalls())
	}
}

func TestLoaderErrorIsRetried(t *testing.T) {
	service := testService(&fakeRuntime{state: runtime.Stopped}, nil)
	loader := &scriptLoader{steps: []loaderStep{
		{err: errors.New("config temporarily unavailable")},
		{service: service},
	}}
	clock := newFakeClock()
	buf := newEventBuffer()
	ctx, cancel := context.WithCancel(context.Background())
	done := runAgent(t, ctx, loader, clock, buf, true)
	defer cancel()

	buf.waitFor(t, 2) // agent_started, observation_error
	ticker := clock.next(t)
	ticker.Tick()
	buf.waitFor(t, 4) // observation_recovered, snapshot

	cancel()
	waitRun(t, done)

	events := decodeEvents(t, buf.all())
	if !equalStrings(types(events), []string{
		EventAgentStarted, EventObservationError, EventObservationRecovered, EventSnapshot, EventAgentStopped,
	}) {
		t.Fatalf("types=%v", types(events))
	}
	if loader.callCount() < 2 {
		t.Fatalf("loader calls=%d", loader.callCount())
	}
}

func TestRuntimeStateErrorIsRetried(t *testing.T) {
	broken := testService(&fakeRuntime{stateErr: errors.New("procfs unavailable")}, nil)
	running := testService(&fakeRuntime{state: runtime.Running, generation: "gen-1", generationOK: true}, nil)
	loader := &scriptLoader{steps: []loaderStep{{service: broken}, {service: running}}}
	clock := newFakeClock()
	buf := newEventBuffer()
	ctx, cancel := context.WithCancel(context.Background())
	done := runAgent(t, ctx, loader, clock, buf, true)
	defer cancel()

	buf.waitFor(t, 2) // agent_started, observation_error
	ticker := clock.next(t)
	ticker.Tick()
	buf.waitFor(t, 4) // observation_recovered, generation_observed

	cancel()
	waitRun(t, done)

	events := decodeEvents(t, buf.all())
	if got := countType(events, EventObservationError); got != 1 {
		t.Fatalf("observation_error count=%d", got)
	}
	if got := countType(events, EventGenerationObserved); got != 1 {
		t.Fatalf("generation_observed count=%d", got)
	}
	if loader.callCount() < 2 {
		t.Fatalf("loader calls=%d", loader.callCount())
	}
}

func TestDuplicateAgentIsRejectedByAgentLock(t *testing.T) {
	dir := privateDir(t)
	t.Setenv("XDG_RUNTIME_DIR", dir)
	lock, err := state.LockAgent("rail")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()

	loader := &scriptLoader{steps: []loaderStep{{service: testService(&fakeRuntime{state: runtime.Stopped}, nil)}}}
	buf := newEventBuffer()
	err = Run(context.Background(), Options{
		ServerName: "rail",
		Interval:   time.Second,
		Loader:     loader.Load,
		Output:     buf,
		Clock:      newFakeClock(),
	})
	if !errors.Is(err, state.ErrBusy) {
		t.Fatalf("err=%v", err)
	}
	if loader.callCount() != 0 {
		t.Fatalf("loader calls=%d", loader.callCount())
	}
	if lines := buf.all(); len(lines) != 0 {
		t.Fatalf("events=%v", lines)
	}
}

func TestJSONEmitsJSONLines(t *testing.T) {
	rt := &fakeRuntime{state: runtime.Running, generation: "gen-1", generationOK: true}
	loader := &scriptLoader{steps: []loaderStep{{service: testService(rt, nil)}}}
	clock := newFakeClock()
	buf := newEventBuffer()
	ctx, cancel := context.WithCancel(context.Background())
	done := runAgent(t, ctx, loader, clock, buf, true)
	defer cancel()

	buf.waitFor(t, 2)
	cancel()
	waitRun(t, done)

	lines := buf.all()
	if len(lines) != 3 {
		t.Fatalf("lines=%d %v", len(lines), lines)
	}
	for _, line := range lines {
		var event Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		if event.Time.IsZero() || event.Server != "rail" || event.Type == "" {
			t.Fatalf("event=%+v", event)
		}
	}
	if strings.Contains(strings.Join(lines, ""), `"exit_code"`) || strings.Contains(strings.Join(lines, ""), `"data"`) {
		t.Fatalf("agent used the one-shot JSON envelope: %v", lines)
	}
}

func TestTextEventFormat(t *testing.T) {
	at := time.Date(2026, 11, 4, 21, 30, 0, 0, time.FixedZone("JST", 9*60*60))
	unknown := formatEvent(Event{Time: at, Server: "rail", Type: EventGenerationExited, Exit: &runtime.ExitStatus{}})
	if want := "2026-11-04T21:30:00+09:00 rail generation_exited exit=unknown\n"; unknown != want {
		t.Fatalf("got %q want %q", unknown, want)
	}
	observed := formatEvent(Event{
		Time: at, Server: "rail", Type: EventGenerationObserved,
		Runtime: runtime.Running, Minecraft: minecraft.Running,
	})
	if want := "2026-11-04T21:30:00+09:00 rail generation_observed runtime=running minecraft=running\n"; observed != want {
		t.Fatalf("got %q want %q", observed, want)
	}
	if got := formatExit(runtime.ExitStatus{Known: true, Code: 0}); got != "0" {
		t.Fatalf("exit=%q", got)
	}
}

type loaderStep struct {
	service app.Service
	err     error
}

type scriptLoader struct {
	mu    sync.Mutex
	steps []loaderStep
	calls int
}

func (l *scriptLoader) Load(string, string) (app.Service, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.steps) == 0 {
		return app.Service{}, errors.New("no service scripted")
	}
	index := l.calls
	l.calls++
	if index >= len(l.steps) {
		index = len(l.steps) - 1
	}
	return l.steps[index].service, l.steps[index].err
}

func (l *scriptLoader) callCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

func testService(rt runtime.Runtime, observer minecraft.Observer) app.Service {
	return app.Service{ServerName: "rail", Runtime: rt, Observer: observer, RuntimeName: "direct"}
}

type fakeRuntime struct {
	state         runtime.State
	stateErr      error
	generation    string
	generationOK  bool
	generationErr error
	waitFn        func(context.Context) (runtime.ExitStatus, error)

	mu          sync.Mutex
	waits       int
	signals     []runtime.Signal
	waitStarted chan struct{}
}

func (r *fakeRuntime) Start(context.Context) error {
	return errors.New("agent must not start the runtime")
}

func (r *fakeRuntime) State(ctx context.Context) (runtime.State, error) {
	if err := ctx.Err(); err != nil {
		return runtime.Unknown, err
	}
	return r.state, r.stateErr
}

func (r *fakeRuntime) Signal(_ context.Context, signal runtime.Signal) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.signals = append(r.signals, signal)
	return nil
}

func (r *fakeRuntime) Wait(ctx context.Context) (runtime.ExitStatus, error) {
	r.mu.Lock()
	r.waits++
	wait := r.waitFn
	started := r.waitStarted
	r.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if wait != nil {
		return wait(ctx)
	}
	<-ctx.Done()
	return runtime.ExitStatus{}, ctx.Err()
}

func (r *fakeRuntime) Generation(ctx context.Context) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	return r.generation, r.generationOK, r.generationErr
}

func (r *fakeRuntime) waitCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.waits
}

func (r *fakeRuntime) signalCalls() []runtime.Signal {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]runtime.Signal(nil), r.signals...)
}

type fakeObserver struct {
	mu     sync.Mutex
	calls  int
	fn     func(int, string) (minecraft.State, error)
	called chan int
}

func (o *fakeObserver) State(ctx context.Context, generation string) (minecraft.State, error) {
	if err := ctx.Err(); err != nil {
		return minecraft.Unknown, err
	}
	o.mu.Lock()
	o.calls++
	call := o.calls
	fn := o.fn
	o.mu.Unlock()
	if fn == nil {
		return minecraft.Unknown, nil
	}
	state, err := fn(call, generation)
	if o.called != nil {
		select {
		case o.called <- call:
		default:
		}
	}
	return state, err
}

func (o *fakeObserver) waitForCall(t *testing.T, want int) {
	t.Helper()
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	for {
		o.mu.Lock()
		calls := o.calls
		o.mu.Unlock()
		if calls >= want {
			return
		}
		select {
		case <-o.called:
		case <-timeout.C:
			t.Fatalf("observer calls=%d want=%d", calls, want)
		}
	}
}

type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	created chan *fakeTicker
}

func newFakeClock() *fakeClock {
	return &fakeClock{
		now:     time.Date(2026, 11, 4, 21, 30, 0, 0, time.UTC),
		created: make(chan *fakeTicker, 64),
	}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTicker(time.Duration) Ticker {
	ticker := &fakeTicker{c: make(chan time.Time, 16)}
	c.created <- ticker
	return ticker
}

func (c *fakeClock) next(t *testing.T) *fakeTicker {
	t.Helper()
	select {
	case ticker := <-c.created:
		return ticker
	case <-time.After(2 * time.Second):
		t.Fatal("agent did not create a ticker")
		return nil
	}
}

type fakeTicker struct {
	c chan time.Time
}

func (t *fakeTicker) C() <-chan time.Time { return t.c }
func (t *fakeTicker) Stop()               {}
func (t *fakeTicker) Tick()               { t.c <- time.Time{} }

type eventBuffer struct {
	mu      sync.Mutex
	lines   []string
	changed chan struct{}
}

func newEventBuffer() *eventBuffer {
	return &eventBuffer{changed: make(chan struct{}, 1)}
}

func (b *eventBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	b.lines = append(b.lines, string(p))
	b.mu.Unlock()
	select {
	case b.changed <- struct{}{}:
	default:
	}
	return len(p), nil
}

func (b *eventBuffer) all() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.lines...)
}

func (b *eventBuffer) waitFor(t *testing.T, want int) {
	t.Helper()
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	for {
		b.mu.Lock()
		lines := len(b.lines)
		b.mu.Unlock()
		if lines >= want {
			return
		}
		select {
		case <-b.changed:
		case <-timeout.C:
			t.Fatalf("events=%d want=%d: %v", lines, want, b.all())
		}
	}
}

func runAgent(t *testing.T, ctx context.Context, loader *scriptLoader, clock *fakeClock, out *eventBuffer, asJSON bool) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			ServerName: "rail",
			Interval:   time.Second,
			Loader:     loader.Load,
			Output:     out,
			JSON:       asJSON,
			Clock:      clock,
			AcquireLock: func(string) (*state.Lock, error) {
				return &state.Lock{}, nil
			},
		})
	}()
	return done
}

func waitRun(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("agent run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent did not stop")
	}
}

func decodeEvents(t *testing.T, lines []string) []Event {
	t.Helper()
	events := make([]Event, 0, len(lines))
	for _, line := range lines {
		var event Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("event line %q: %v", line, err)
		}
		events = append(events, event)
	}
	return events
}

func types(events []Event) []string {
	out := make([]string, len(events))
	for i, event := range events {
		out[i] = event.Type
	}
	return out
}

func countType(events []Event, want string) int {
	count := 0
	for _, event := range events {
		if event.Type == want {
			count++
		}
	}
	return count
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func privateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}
