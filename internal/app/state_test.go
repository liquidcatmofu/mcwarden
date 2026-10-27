package app

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

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
	f.state = runtime.Stopped
	return runtime.ExitStatus{Known: true}, nil
}

func (f *stateRuntime) Identity() (runtime.Identity, bool) {
	return f.identity, f.hasID
}

func stateService(t *testing.T, rt runtime.Runtime, server string) Service {
	t.Helper()
	stateDir := t.TempDir()
	runtimeDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", stateDir)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
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
	result, err := service.Start(context.Background(), StartOptions{NoWait: true})
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
	if _, err := service.Start(context.Background(), StartOptions{NoWait: true}); !errors.Is(err, ErrAlreadyRunning) {
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
	if _, err := service.Start(context.Background(), StartOptions{NoWait: true}); !errors.Is(err, ErrStaleState) {
		t.Fatalf("err=%v", err)
	}
	if rt.started != 0 {
		t.Fatalf("started=%d", rt.started)
	}
}

func TestStartRefusesRunningRuntimeWithoutState(t *testing.T) {
	rt := &stateRuntime{state: runtime.Running, hasID: true, identity: runtime.Identity{PID: os.Getpid(), PGID: os.Getpid(), StartTime: 1}}
	service := stateService(t, rt, "rail")
	if _, err := service.Start(context.Background(), StartOptions{NoWait: true}); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("err=%v", err)
	}
	if rt.started != 0 {
		t.Fatalf("started=%d", rt.started)
	}
}

func TestStartRollsBackWithoutIdentity(t *testing.T) {
	rt := &stateRuntime{state: runtime.Stopped, hasID: false}
	service := stateService(t, rt, "rail")
	if _, err := service.Start(context.Background(), StartOptions{NoWait: true}); !errors.Is(err, ErrStateUnknown) {
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

func saveStaleState(t *testing.T, service Service) *state.Store {
	t.Helper()
	store, err := service.StateStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save("direct", state.Identity{PID: 1 << 30, PGID: 1 << 30, StartTime: 1}); err != nil {
		t.Fatal(err)
	}
	return store
}

func requireAbsent(t *testing.T, store *state.Store) {
	t.Helper()
	inspection, err := store.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Status != state.Absent {
		t.Fatalf("inspection=%+v", inspection)
	}
}

// TestStopCleansExitedState verifies that a confirmed exit removes the old
// identity during the same operation, so no stale record is left behind.
func TestStopCleansExitedState(t *testing.T) {
	rt := &stateRuntime{state: runtime.Running}
	service := stateService(t, rt, "rail")
	service.Minecraft = &fake{}
	store := saveStaleState(t, service)

	if _, err := service.Stop(context.Background(), StopOptions{Timeout: time.Second}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	requireAbsent(t, store)
}

// TestRestartCleansOldStateBeforeStart covers the restart path that previously
// failed: the old generation exits, its state is cleaned, and the new
// generation is started and recorded under the same lock.
func TestRestartCleansOldStateBeforeStart(t *testing.T) {
	current, err := currentIdentity()
	if err != nil {
		t.Fatal(err)
	}
	rt := &stateRuntime{
		state: runtime.Running,
		hasID: true,
		identity: runtime.Identity{
			PID: current.PID, PGID: current.PGID, StartTime: current.StartTime, Owned: true,
		},
	}
	service := stateService(t, rt, "rail")
	service.Minecraft = &fake{}
	store := saveStaleState(t, service)

	if _, err := service.Restart(context.Background(), RestartOptions{NoWait: true, StopTimeout: time.Second}); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if rt.started != 1 {
		t.Fatalf("started=%d", rt.started)
	}
	inspection, err := store.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Status != state.Managed || inspection.Record == nil || inspection.Record.PID != current.PID {
		t.Fatalf("inspection=%+v", inspection)
	}
}

// TestStopRefusesToCleanManagedState verifies that a confirmed wait on one
// generation never deletes state that still describes another live process.
func TestStopRefusesToCleanManagedState(t *testing.T) {
	rt := &stateRuntime{state: runtime.Running}
	service := stateService(t, rt, "rail")
	service.Minecraft = &fake{}
	store, err := service.StateStore()
	if err != nil {
		t.Fatal(err)
	}
	current, err := currentIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save("direct", current); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Stop(context.Background(), StopOptions{Timeout: time.Second}); !errors.Is(err, state.ErrManaged) {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(store.Path()); err != nil {
		t.Fatalf("managed state was removed: %v", err)
	}
}

// TestMutationsLockServer verifies that every mutating operation takes the
// per-server lock before touching state or the runtime.
func TestMutationsLockServer(t *testing.T) {
	service := stateService(t, &stateRuntime{state: runtime.Stopped}, "rail")
	store, err := service.StateStore()
	if err != nil {
		t.Fatal(err)
	}
	lock, err := store.Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()

	ctx := context.Background()
	operations := map[string]func() error{
		"start": func() error {
			_, err := service.Start(ctx, StartOptions{NoWait: true})
			return err
		},
		"stop": func() error {
			_, err := service.Stop(ctx, StopOptions{Timeout: time.Second})
			return err
		},
		"restart": func() error {
			_, err := service.Restart(ctx, RestartOptions{NoWait: true, StopTimeout: time.Second})
			return err
		},
		"clean": func() error {
			_, err := service.CleanState(ctx)
			return err
		},
	}
	for name, run := range operations {
		t.Run(name, func(t *testing.T) {
			if err := run(); !errors.Is(err, state.ErrBusy) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}
