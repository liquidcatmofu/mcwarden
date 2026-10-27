package app

import (
	"context"
	"errors"
	"os"
	"testing"

	hostlinux "github.com/liquidcatmofu/mcwarden/internal/host/linux"
	"github.com/liquidcatmofu/mcwarden/internal/runtime"
	"github.com/liquidcatmofu/mcwarden/internal/state"
)

func currentIdentity() (state.Identity, error) {
	stat, err := hostlinux.Read(os.Getpid())
	if err != nil {
		return state.Identity{}, err
	}
	return state.Identity{PID: stat.PID, PGID: stat.PGID, StartTime: stat.StartTime, Owned: true}, nil
}

type stateRuntime struct {
	state    runtime.State
	identity runtime.Identity
	hasID    bool
	started  int
	signals  int
}

func (f *stateRuntime) Start(context.Context) error {
	f.started++
	f.state = runtime.Running
	return nil
}

func (f *stateRuntime) State(context.Context) (runtime.State, error) { return f.state, nil }

func (f *stateRuntime) Signal(context.Context, runtime.Signal) error {
	f.signals++
	f.state = runtime.Stopped
	return nil
}

func (f *stateRuntime) Wait(context.Context) (runtime.ExitStatus, error) {
	return runtime.ExitStatus{Known: true}, nil
}

func (f *stateRuntime) Identity() (runtime.Identity, bool) {
	return f.identity, f.hasID
}

func stateService(t *testing.T, rt runtime.Runtime, server string) Service {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", dir)
	return Service{
		Runtime:     rt,
		RuntimeName: "direct",
		StateStore:  func() (*state.Store, error) { return state.Open(server) },
	}
}

func TestStartPersistsIdentity(t *testing.T) {
	identity, err := currentIdentity()
	if err != nil {
		t.Fatal(err)
	}
	rt := &stateRuntime{
		state: runtime.Stopped,
		hasID: true,
		identity: runtime.Identity{
			PID: identity.PID, PGID: identity.PGID, StartTime: identity.StartTime, Owned: true,
		},
	}
	service := stateService(t, rt, "rail")
	result, err := service.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if rt.started != 1 {
		t.Fatalf("started=%d", rt.started)
	}
	if len(result.Events) != 2 || result.Events[0] != StartRequested || result.Events[1] != ProcessStarted {
		t.Fatalf("events=%v", result.Events)
	}
	store, err := service.StateStore()
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := store.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Status != state.Managed || inspection.Record == nil {
		t.Fatalf("inspection=%+v", inspection)
	}
	if inspection.Record.PID != identity.PID || inspection.Record.Runtime != "direct" || !inspection.Record.Owned {
		t.Fatalf("record=%+v", inspection.Record)
	}
}

func TestStartRefusesManagedState(t *testing.T) {
	rt := &stateRuntime{state: runtime.Stopped, hasID: true, identity: runtime.Identity{PID: os.Getpid(), PGID: os.Getpid(), StartTime: 1}}
	service := stateService(t, rt, "rail")
	store, err := service.StateStore()
	if err != nil {
		t.Fatal(err)
	}
	real, err := currentIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save("direct", real); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Start(context.Background()); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("err=%v", err)
	}
	if rt.started != 0 {
		t.Fatalf("started=%d", rt.started)
	}
}

func TestStartRefusesStaleState(t *testing.T) {
	rt := &stateRuntime{state: runtime.Stopped, hasID: true, identity: runtime.Identity{PID: os.Getpid(), PGID: os.Getpid(), StartTime: 1}}
	service := stateService(t, rt, "rail")
	store, err := service.StateStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save("direct", state.Identity{PID: 1 << 30, PGID: 1 << 30, StartTime: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Start(context.Background()); !errors.Is(err, ErrStaleState) {
		t.Fatalf("err=%v", err)
	}
	if rt.started != 0 {
		t.Fatalf("started=%d", rt.started)
	}
}

func TestStartRefusesRunningRuntimeWithoutState(t *testing.T) {
	rt := &stateRuntime{state: runtime.Running, hasID: true, identity: runtime.Identity{PID: os.Getpid(), PGID: os.Getpid(), StartTime: 1}}
	service := stateService(t, rt, "rail")
	if _, err := service.Start(context.Background()); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("err=%v", err)
	}
	if rt.started != 0 {
		t.Fatalf("started=%d", rt.started)
	}
}

func TestStartRollsBackWithoutIdentity(t *testing.T) {
	rt := &stateRuntime{state: runtime.Stopped, hasID: false}
	service := stateService(t, rt, "rail")
	if _, err := service.Start(context.Background()); !errors.Is(err, ErrStateUnknown) {
		t.Fatalf("err=%v", err)
	}
	if rt.started != 1 || rt.signals != 1 {
		t.Fatalf("started=%d signals=%d", rt.started, rt.signals)
	}
	store, err := service.StateStore()
	if err != nil {
		t.Fatal(err)
	}
	if inspection, err := store.Inspect(); err != nil || inspection.Status != state.Absent {
		t.Fatalf("inspection=%+v err=%v", inspection, err)
	}
}

func TestCleanStateRemovesStale(t *testing.T) {
	rt := &stateRuntime{state: runtime.Stopped}
	service := stateService(t, rt, "rail")
	store, err := service.StateStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save("direct", state.Identity{PID: 1 << 30, PGID: 1 << 30, StartTime: 1}); err != nil {
		t.Fatal(err)
	}
	inspection, err := service.CleanState(context.Background())
	if err != nil {
		t.Fatalf("clean: %v", err)
	}
	if inspection.Status != state.Absent {
		t.Fatalf("inspection=%+v", inspection)
	}
	if _, err := os.Stat(store.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state still present: %v", err)
	}
	if _, err := service.CleanState(context.Background()); err != nil {
		t.Fatalf("clean absent: %v", err)
	}
}

func TestCleanStateRefusesManaged(t *testing.T) {
	rt := &stateRuntime{state: runtime.Running}
	service := stateService(t, rt, "rail")
	store, err := service.StateStore()
	if err != nil {
		t.Fatal(err)
	}
	identity, err := currentIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save("direct", identity); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CleanState(context.Background()); !errors.Is(err, state.ErrManaged) {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(store.Path()); err != nil {
		t.Fatalf("state was removed: %v", err)
	}
}

func TestStateStatusAbsent(t *testing.T) {
	service := stateService(t, &stateRuntime{state: runtime.Stopped}, "rail")
	inspection, err := service.StateStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Status != state.Absent {
		t.Fatalf("inspection=%+v", inspection)
	}
}

func TestStateWithoutStoreIsUnsupported(t *testing.T) {
	service := Service{}
	if _, err := service.StateStatus(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err=%v", err)
	}
}
