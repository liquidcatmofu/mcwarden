package app

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/liquidcatmofu/mcwarden/internal/runtime"
	"github.com/liquidcatmofu/mcwarden/internal/state"
)

func newOperationStore(t *testing.T, server string) *state.OperationStore {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", dir)
	store, err := state.OpenOperations(server)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func loadOperations(t *testing.T, store *state.OperationStore) state.OperationRecord {
	t.Helper()
	rec, ok, err := store.Load()
	if err != nil {
		t.Fatalf("load operations: %v", err)
	}
	if !ok {
		t.Fatal("operation record was not created")
	}
	return rec
}

func TestStartPersistsDesiredState(t *testing.T) {
	tests := []struct {
		name     string
		startErr error
	}{
		{name: "success"},
		{name: "start failure", startErr: errTransport},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newOperationStore(t, "rail")
			f := &fake{state: runtime.Stopped, startErr: tt.startErr}
			_, err := (Service{Runtime: f, Minecraft: f, OperationStore: store}).Start(context.Background(), StartOptions{NoWait: true})
			if !errors.Is(err, tt.startErr) {
				t.Fatalf("err=%v want %v", err, tt.startErr)
			}
			rec := loadOperations(t, store)
			if rec.Desired != state.DesiredRunning {
				t.Fatalf("desired=%q", rec.Desired)
			}
			if len(rec.ExitIntents) != 0 {
				t.Fatalf("start wrote exit intents: %+v", rec.ExitIntents)
			}
		})
	}
}

func TestStopPersistsDesiredAndAcceptedIntent(t *testing.T) {
	store := newOperationStore(t, "rail")
	var atCommand state.OperationRecord
	f := &fake{
		state: runtime.Running, generation: "gen-a", generationOK: true,
		onCommand: func() { atCommand = loadOperations(t, store) },
	}
	result, err := (Service{Runtime: f, Minecraft: f, OperationStore: store}).Stop(context.Background(), StopOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if atCommand.Desired != state.DesiredStopped {
		t.Fatalf("desired at command=%q", atCommand.Desired)
	}
	if len(atCommand.ExitIntents) != 1 || atCommand.ExitIntents[0].Phase != state.IntentPrepared ||
		atCommand.ExitIntents[0].Generation != "gen-a" || atCommand.ExitIntents[0].Operation != state.ExitStop {
		t.Fatalf("intent at command=%+v", atCommand.ExitIntents)
	}

	rec := loadOperations(t, store)
	if rec.Desired != state.DesiredStopped {
		t.Fatalf("desired=%q", rec.Desired)
	}
	if len(rec.ExitIntents) != 1 {
		t.Fatalf("intents=%+v", rec.ExitIntents)
	}
	intent := rec.ExitIntents[0]
	if intent.Generation != "gen-a" || intent.Operation != state.ExitStop || intent.Phase != state.IntentAccepted || intent.AcceptedAt == nil {
		t.Fatalf("intent=%+v", intent)
	}
	if !equalEvents(result.Events, []Event{StopRequested, Stopped}) {
		t.Fatalf("events=%v", result.Events)
	}
}

func TestStopCommandFailureKeepsDesiredAndRemovesPrepared(t *testing.T) {
	store := newOperationStore(t, "rail")
	f := &fake{
		state: runtime.Running, generation: "gen-a", generationOK: true,
		commandErr: errTransport,
	}
	_, err := (Service{Runtime: f, Minecraft: f, OperationStore: store}).Stop(context.Background(), StopOptions{Timeout: time.Second})
	if !errors.Is(err, errTransport) {
		t.Fatalf("err=%v", err)
	}
	rec := loadOperations(t, store)
	if rec.Desired != state.DesiredStopped {
		t.Fatalf("desired=%q", rec.Desired)
	}
	if len(rec.ExitIntents) != 0 {
		t.Fatalf("prepared intent was not removed: %+v", rec.ExitIntents)
	}
}

func TestStopWithoutGenerationDoesNotInventIntent(t *testing.T) {
	store := newOperationStore(t, "rail")
	f := &fake{state: runtime.Running, generationOK: false}
	if _, err := (Service{Runtime: f, Minecraft: f, OperationStore: store}).Stop(context.Background(), StopOptions{Timeout: time.Second}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	rec := loadOperations(t, store)
	if rec.Desired != state.DesiredStopped {
		t.Fatalf("desired=%q", rec.Desired)
	}
	if len(rec.ExitIntents) != 0 {
		t.Fatalf("intent was invented without a generation: %+v", rec.ExitIntents)
	}
}

func TestRestartPersistsDesiredAndKeepsAcceptedIntent(t *testing.T) {
	store := newOperationStore(t, "rail")
	f := &fake{state: runtime.Running, generation: "gen-a", generationOK: true}
	result, err := (Service{Runtime: f, Minecraft: f, OperationStore: store}).Restart(context.Background(), RestartOptions{
		NoWait: true, StopTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if !equalEvents(result.Events, []Event{StopRequested, Stopped, StartRequested, ProcessStarted}) {
		t.Fatalf("events=%v", result.Events)
	}
	rec := loadOperations(t, store)
	if rec.Desired != state.DesiredRunning {
		t.Fatalf("desired=%q", rec.Desired)
	}
	if len(rec.ExitIntents) != 1 {
		t.Fatalf("intents=%+v", rec.ExitIntents)
	}
	intent := rec.ExitIntents[0]
	if intent.Generation != "gen-a" || intent.Operation != state.ExitRestart || intent.Phase != state.IntentAccepted {
		t.Fatalf("intent=%+v", intent)
	}
}

func TestRestartFailuresKeepDesiredRunning(t *testing.T) {
	tests := []struct {
		name       string
		commandErr error
		startErr   error
		wantIntent bool
	}{
		{name: "stop failure", commandErr: errTransport},
		{name: "startup failure", startErr: errTransport, wantIntent: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newOperationStore(t, "rail")
			f := &fake{
				state: runtime.Running, generation: "gen-a", generationOK: true,
				commandErr: tt.commandErr, startErr: tt.startErr,
			}
			_, err := (Service{Runtime: f, Minecraft: f, OperationStore: store}).Restart(context.Background(), RestartOptions{
				NoWait: true, StopTimeout: time.Second,
			})
			if !errors.Is(err, errTransport) {
				t.Fatalf("err=%v", err)
			}
			rec := loadOperations(t, store)
			if rec.Desired != state.DesiredRunning {
				t.Fatalf("desired=%q", rec.Desired)
			}
			if got := len(rec.ExitIntents) == 1; got != tt.wantIntent {
				t.Fatalf("intents=%+v want present=%v", rec.ExitIntents, tt.wantIntent)
			}
		})
	}
}

// failingLedger wraps a real operation store and injects persistence failures
// after an external side effect.
type failingLedger struct {
	store        *state.OperationStore
	failAccept   bool
	failRemove   bool
	rejectedOpen bool
}

func (l *failingLedger) Load() (state.OperationRecord, bool, error) {
	return l.store.Load()
}

func (l *failingLedger) Update(ctx context.Context, fn func(state.OperationRecord, bool) (state.OperationRecord, bool, error)) (state.OperationRecord, bool, error) {
	return l.store.Update(ctx, func(current state.OperationRecord, exists bool) (state.OperationRecord, bool, error) {
		rec := cloneOperationRecord(current)
		next, write, err := fn(rec, exists)
		if err != nil || !write {
			return next, write, err
		}
		if l.failAccept && acceptedCount(next) > acceptedCount(current) {
			return current, false, errors.New("accepted persistence failed")
		}
		if l.failRemove && len(next.ExitIntents) < len(current.ExitIntents) {
			return current, false, errors.New("prepared removal failed")
		}
		return next, write, nil
	})
}

func cloneOperationRecord(rec state.OperationRecord) state.OperationRecord {
	rec.ExitIntents = append([]state.ExitIntent(nil), rec.ExitIntents...)
	return rec
}

func acceptedCount(rec state.OperationRecord) int {
	count := 0
	for _, intent := range rec.ExitIntents {
		if intent.Phase == state.IntentAccepted {
			count++
		}
	}
	return count
}

func TestAcceptedPersistenceFailureDoesNotAbortLifecycle(t *testing.T) {
	tests := []struct {
		name string
		run  func(Service) (Result, error)
	}{
		{
			name: "stop still waits",
			run: func(s Service) (Result, error) {
				return s.Stop(context.Background(), StopOptions{Timeout: time.Second})
			},
		},
		{
			name: "restart still waits and starts",
			run: func(s Service) (Result, error) {
				return s.Restart(context.Background(), RestartOptions{NoWait: true, StopTimeout: time.Second})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newOperationStore(t, "rail")
			ledger := &failingLedger{store: store, failAccept: true}
			f := &fake{state: runtime.Running, generation: "gen-a", generationOK: true}
			result, err := tt.run(Service{Runtime: f, Minecraft: f, ledger: ledger})
			if err != nil {
				t.Fatalf("lifecycle aborted after accepted persistence failure: %v", err)
			}
			if !containsCall(f.calls, "wait") {
				t.Fatalf("Wait did not run: %v", f.calls)
			}
			if tt.name == "restart still waits and starts" && !containsCall(f.calls, "start") {
				t.Fatalf("Restart did not start after accepted persistence failure: %v", f.calls)
			}
			if !containsEvent(result.Events, Stopped) {
				t.Fatalf("events=%v", result.Events)
			}
			rec := loadOperations(t, store)
			if rec.Desired != state.DesiredStopped && tt.name == "stop still waits" {
				t.Fatalf("desired=%q", rec.Desired)
			}
			if rec.Desired != state.DesiredRunning && tt.name == "restart still waits and starts" {
				t.Fatalf("desired=%q", rec.Desired)
			}
			if len(rec.ExitIntents) != 1 || rec.ExitIntents[0].Phase != state.IntentPrepared {
				t.Fatalf("accepted persistence failure must leave prepared: %+v", rec.ExitIntents)
			}
		})
	}
}

func TestPreparedRemovalFailureLeavesPrepared(t *testing.T) {
	store := newOperationStore(t, "rail")
	ledger := &failingLedger{store: store, failRemove: true}
	f := &fake{
		state: runtime.Running, generation: "gen-a", generationOK: true,
		commandErr: errTransport,
	}
	_, err := (Service{Runtime: f, Minecraft: f, ledger: ledger}).Stop(context.Background(), StopOptions{Timeout: time.Second})
	if !errors.Is(err, errTransport) {
		t.Fatalf("err=%v", err)
	}
	rec := loadOperations(t, store)
	if rec.Desired != state.DesiredStopped {
		t.Fatalf("desired=%q", rec.Desired)
	}
	if len(rec.ExitIntents) != 1 || rec.ExitIntents[0].Phase != state.IntentPrepared {
		t.Fatalf("prepared intent must remain: %+v", rec.ExitIntents)
	}
}

func containsCall(calls []string, want string) bool {
	for _, call := range calls {
		if call == want {
			return true
		}
	}
	return false
}

func containsEvent(events []Event, want Event) bool {
	for _, event := range events {
		if event == want {
			return true
		}
	}
	return false
}

func equalEvents(got, want []Event) bool {
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
