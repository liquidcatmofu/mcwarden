package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/liquidcatmofu/mcwarden/internal/app"
	"github.com/liquidcatmofu/mcwarden/internal/runtime"
	"github.com/liquidcatmofu/mcwarden/internal/state"
)

func newOperationStoreForAgent(t *testing.T, server string) *state.OperationStore {
	t.Helper()
	dir := privateDir(t)
	t.Setenv("XDG_STATE_HOME", dir)
	store, err := state.OpenOperations(server)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func seedOperations(t *testing.T, store *state.OperationStore, desired state.DesiredState, intents ...state.ExitIntent) {
	t.Helper()
	if _, _, err := store.Update(context.Background(), func(state.OperationRecord, bool) (state.OperationRecord, bool, error) {
		return state.OperationRecord{Desired: desired, ExitIntents: intents}, true, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func acceptedIntent(generation string, operation state.ExitOperation) state.ExitIntent {
	now := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	return state.ExitIntent{
		Generation: generation,
		Operation:  operation,
		Phase:      state.IntentAccepted,
		PreparedAt: now,
		AcceptedAt: &now,
	}
}

func preparedIntent(generation string, operation state.ExitOperation) state.ExitIntent {
	return state.ExitIntent{
		Generation: generation,
		Operation:  operation,
		Phase:      state.IntentPrepared,
		PreparedAt: time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC),
	}
}

func TestClassifyExit(t *testing.T) {
	t.Run("accepted stop is intentional", func(t *testing.T) {
		store := newOperationStoreForAgent(t, "rail")
		seedOperations(t, store, state.DesiredStopped, acceptedIntent("gen-a", state.ExitStop))
		class, op, err := (&agent{}).classifyExit(app.Service{OperationStore: store}, "gen-a")
		if err != nil || class != ExitIntentional || op != state.ExitStop {
			t.Fatalf("class=%s op=%s err=%v", class, op, err)
		}
	})

	t.Run("accepted restart is intentional", func(t *testing.T) {
		store := newOperationStoreForAgent(t, "rail")
		seedOperations(t, store, state.DesiredRunning, acceptedIntent("gen-a", state.ExitRestart))
		class, op, err := (&agent{}).classifyExit(app.Service{OperationStore: store}, "gen-a")
		if err != nil || class != ExitIntentional || op != state.ExitRestart {
			t.Fatalf("class=%s op=%s err=%v", class, op, err)
		}
	})

	t.Run("prepared is unknown", func(t *testing.T) {
		store := newOperationStoreForAgent(t, "rail")
		seedOperations(t, store, state.DesiredStopped, preparedIntent("gen-a", state.ExitStop))
		class, op, err := (&agent{}).classifyExit(app.Service{OperationStore: store}, "gen-a")
		if err != nil || class != ExitUnknown || op != state.ExitStop {
			t.Fatalf("class=%s op=%s err=%v", class, op, err)
		}
	})

	t.Run("no matching generation is unexpected", func(t *testing.T) {
		store := newOperationStoreForAgent(t, "rail")
		seedOperations(t, store, state.DesiredRunning, acceptedIntent("gen-a", state.ExitRestart))
		class, op, err := (&agent{}).classifyExit(app.Service{OperationStore: store}, "gen-b")
		if err != nil || class != ExitUnexpected || op != "" {
			t.Fatalf("class=%s op=%s err=%v", class, op, err)
		}
	})

	t.Run("absent ledger is unexpected", func(t *testing.T) {
		store := newOperationStoreForAgent(t, "rail")
		class, op, err := (&agent{}).classifyExit(app.Service{OperationStore: store}, "gen-a")
		if err != nil || class != ExitUnexpected || op != "" {
			t.Fatalf("class=%s op=%s err=%v", class, op, err)
		}
	})

	t.Run("old generation remains after newer intent", func(t *testing.T) {
		store := newOperationStoreForAgent(t, "rail")
		seedOperations(t, store, state.DesiredRunning,
			acceptedIntent("gen-a", state.ExitRestart),
			acceptedIntent("gen-b", state.ExitStop),
		)
		class, op, err := (&agent{}).classifyExit(app.Service{OperationStore: store}, "gen-a")
		if err != nil || class != ExitIntentional || op != state.ExitRestart {
			t.Fatalf("gen-a class=%s op=%s err=%v", class, op, err)
		}
		class, op, err = (&agent{}).classifyExit(app.Service{OperationStore: store}, "gen-b")
		if err != nil || class != ExitIntentional || op != state.ExitStop {
			t.Fatalf("gen-b class=%s op=%s err=%v", class, op, err)
		}
	})

	t.Run("corrupt ledger is unknown with an error", func(t *testing.T) {
		store := newOperationStoreForAgent(t, "rail")
		if err := os.MkdirAll(filepath.Dir(store.Path()), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(store.Path(), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		class, op, err := (&agent{}).classifyExit(app.Service{OperationStore: store}, "gen-a")
		if !errors.Is(err, state.ErrCorrupt) || class != ExitUnknown || op != "" {
			t.Fatalf("class=%s op=%s err=%v", class, op, err)
		}
	})

	t.Run("ledger opener error is unknown", func(t *testing.T) {
		service := app.Service{Operations: func() (*state.OperationStore, error) {
			return nil, errors.New("unavailable")
		}}
		class, op, err := (&agent{}).classifyExit(service, "gen-a")
		if err == nil || class != ExitUnknown || op != "" {
			t.Fatalf("class=%s op=%s err=%v", class, op, err)
		}
	})

	t.Run("no ledger configured is unexpected", func(t *testing.T) {
		class, op, err := (&agent{}).classifyExit(app.Service{}, "gen-a")
		if err != nil || class != ExitUnexpected || op != "" {
			t.Fatalf("class=%s op=%s err=%v", class, op, err)
		}
	})
}

func TestGenerationExitedCarriesClassificationAndRawExit(t *testing.T) {
	store := newOperationStoreForAgent(t, "rail")
	seedOperations(t, store, state.DesiredRunning, acceptedIntent("gen-1", state.ExitRestart))

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
		{service: app.Service{ServerName: "rail", Runtime: oldRuntime, OperationStore: store}},
		{service: app.Service{ServerName: "rail", Runtime: newRuntime, OperationStore: store}},
	}}
	clock := newFakeClock()
	buf := newEventBuffer()
	ctx, cancel := context.WithCancel(context.Background())
	done := runAgent(t, ctx, loader, clock, buf, true)
	defer cancel()

	buf.waitFor(t, 4)
	select {
	case <-newRuntime.waitStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement generation was not waited on")
	}
	cancel()
	waitRun(t, done)

	events := decodeEvents(t, buf.all())
	var exited *Event
	for i := range events {
		if events[i].Type == EventGenerationExited {
			exited = &events[i]
			break
		}
	}
	if exited == nil {
		t.Fatalf("types=%v", types(events))
	}
	if exited.Generation != "gen-1" || exited.Classification != ExitIntentional || exited.Operation != state.ExitRestart {
		t.Fatalf("exited=%+v", *exited)
	}
	if exited.Exit == nil || !exited.Exit.Known || exited.Exit.Code != 1 {
		t.Fatalf("raw exit was not preserved: %+v", exited.Exit)
	}
}

func TestGenerationExitedLedgerErrorIsUnknownAndStillEmitted(t *testing.T) {
	store := newOperationStoreForAgent(t, "rail")
	if err := os.MkdirAll(filepath.Dir(store.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.Path(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	oldRuntime := &fakeRuntime{
		state: runtime.Running, generation: "gen-1", generationOK: true,
		waitFn: func(context.Context) (runtime.ExitStatus, error) {
			return runtime.ExitStatus{Known: true, Code: 0}, nil
		},
	}
	newRuntime := &fakeRuntime{
		state: runtime.Running, generation: "gen-2", generationOK: true,
		waitStarted: make(chan struct{}, 1),
	}
	loader := &scriptLoader{steps: []loaderStep{
		{service: app.Service{ServerName: "rail", Runtime: oldRuntime, OperationStore: store}},
		{service: app.Service{ServerName: "rail", Runtime: newRuntime, OperationStore: store}},
	}}
	clock := newFakeClock()
	buf := newEventBuffer()
	ctx, cancel := context.WithCancel(context.Background())
	done := runAgent(t, ctx, loader, clock, buf, true)
	defer cancel()

	buf.waitFor(t, 5) // exit, observation_error, recovered, new generation observed
	cancel()
	waitRun(t, done)

	events := decodeEvents(t, buf.all())
	var exited *Event
	for i := range events {
		if events[i].Type == EventGenerationExited {
			exited = &events[i]
			break
		}
	}
	if exited == nil {
		t.Fatalf("generation_exited was lost: %v", types(events))
	}
	if exited.Classification != ExitUnknown || exited.Operation != "" {
		t.Fatalf("exited=%+v", *exited)
	}
	if countType(events, EventObservationError) < 1 {
		t.Fatalf("ledger read error was not reported: %v", types(events))
	}
}

func TestGenerationExitedTextFormat(t *testing.T) {
	at := time.Date(2026, 11, 5, 9, 0, 0, 0, time.FixedZone("JST", 9*60*60))
	tests := []struct {
		name  string
		event Event
		want  string
	}{
		{
			name: "intentional restart",
			event: Event{
				Time: at, Server: "rail", Type: EventGenerationExited,
				Classification: ExitIntentional, Operation: state.ExitRestart,
				Exit: &runtime.ExitStatus{Known: true, Code: 0},
			},
			want: "2026-11-05T09:00:00+09:00 rail generation_exited classification=intentional operation=restart exit=0\n",
		},
		{
			name: "unexpected",
			event: Event{
				Time: at, Server: "rail", Type: EventGenerationExited,
				Classification: ExitUnexpected,
				Exit:           &runtime.ExitStatus{Known: true, Code: 1},
			},
			want: "2026-11-05T09:00:00+09:00 rail generation_exited classification=unexpected exit=1\n",
		},
		{
			name: "unknown prepared",
			event: Event{
				Time: at, Server: "rail", Type: EventGenerationExited,
				Classification: ExitUnknown, Operation: state.ExitStop,
				Exit: &runtime.ExitStatus{},
			},
			want: "2026-11-05T09:00:00+09:00 rail generation_exited classification=unknown operation=stop exit=unknown\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatEvent(tt.event); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}
