//go:build linux

package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newOperationStore(t *testing.T, server string) *OperationStore {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", dir)
	store, err := OpenOperations(server)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestOperationSaveCreatesPrivateDirectory(t *testing.T) {
	store := newOperationStore(t, "rail")
	rec, written, err := store.Update(context.Background(), func(OperationRecord, bool) (OperationRecord, bool, error) {
		return OperationRecord{Desired: DesiredRunning}, true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !written || rec.Desired != DesiredRunning {
		t.Fatalf("rec=%+v written=%v", rec, written)
	}
	info, err := os.Stat(filepath.Dir(store.Path()))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode=%04o want 0700", info.Mode().Perm())
	}
	info, err = os.Stat(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode=%04o want 0600", info.Mode().Perm())
	}
	if !strings.HasSuffix(store.Path(), filepath.Join("operations", "rail.json")) {
		t.Fatalf("path=%s", store.Path())
	}
}

func TestOperationOpenRejectsRelativeStateDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "relative/path")
	if _, err := OpenOperations("rail"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err=%v", err)
	}
}

func TestOperationUpdateSerializesWriters(t *testing.T) {
	store := newOperationStore(t, "rail")
	other, err := OpenOperations("rail")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Update(context.Background(), func(OperationRecord, bool) (OperationRecord, bool, error) {
		return OperationRecord{Desired: DesiredStopped}, true, nil
	}); err != nil {
		t.Fatal(err)
	}

	const writers = 8
	done := make(chan error, writers)
	for i := 0; i < writers; i++ {
		go func(store *OperationStore) {
			_, _, err := store.Update(context.Background(), func(rec OperationRecord, exists bool) (OperationRecord, bool, error) {
				if !exists {
					return OperationRecord{}, false, errors.New("record disappeared")
				}
				rec.ExitIntents = append(rec.ExitIntents, ExitIntent{
					Generation: "gen",
					Operation:  ExitStop,
					Phase:      IntentPrepared,
					PreparedAt: time.Now().UTC(),
				})
				return rec, true, nil
			})
			done <- err
		}([]*OperationStore{store, other}[i%2])
	}
	for i := 0; i < writers; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}

	rec, ok, err := store.Load()
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	if len(rec.ExitIntents) != writers {
		t.Fatalf("intents=%d want %d (lost update)", len(rec.ExitIntents), writers)
	}
}

func TestOperationUpdateWithoutWriteLeavesRecord(t *testing.T) {
	store := newOperationStore(t, "rail")
	if _, _, err := store.Update(context.Background(), func(OperationRecord, bool) (OperationRecord, bool, error) {
		return OperationRecord{Desired: DesiredRunning}, true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Update(context.Background(), func(rec OperationRecord, exists bool) (OperationRecord, bool, error) {
		return OperationRecord{Desired: DesiredStopped}, false, nil
	}); err != nil {
		t.Fatal(err)
	}
	rec, _, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if rec.Desired != DesiredRunning {
		t.Fatalf("record overwritten: %+v", rec)
	}
}

func TestOperationPrunesOldIntentsOnlyOnAppend(t *testing.T) {
	store := newOperationStore(t, "rail")
	now := time.Now().UTC()
	if _, _, err := store.Update(context.Background(), func(OperationRecord, bool) (OperationRecord, bool, error) {
		rec := OperationRecord{Desired: DesiredRunning}
		for i := 0; i < maxExitIntents+1; i++ {
			rec.ExitIntents = append(rec.ExitIntents, ExitIntent{
				Generation: "gen-" + string(rune('A'+i%26)) + "-" + time.Duration(i).String(),
				Operation:  ExitStop,
				Phase:      IntentPrepared,
				PreparedAt: now,
			})
		}
		return rec, true, nil
	}); err != nil {
		t.Fatal(err)
	}
	rec, _, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.ExitIntents) != maxExitIntents {
		t.Fatalf("intents=%d want %d", len(rec.ExitIntents), maxExitIntents)
	}
	if strings.Contains(rec.ExitIntents[0].Generation, "gen-A") {
		t.Fatalf("oldest intent was not pruned: %+v", rec.ExitIntents[0])
	}
}

func TestOperationLoadCorruptFailsClosed(t *testing.T) {
	tests := map[string]string{
		"not json":         `{not json`,
		"wrong version":    `{"version":2,"server":"rail","desired":"running","updated_at":"2026-11-05T00:00:00Z","exit_intents":[]}`,
		"wrong server":     `{"version":1,"server":"other","desired":"running","updated_at":"2026-11-05T00:00:00Z","exit_intents":[]}`,
		"unknown desired":  `{"version":1,"server":"rail","desired":"paused","updated_at":"2026-11-05T00:00:00Z","exit_intents":[]}`,
		"missing gen":      `{"version":1,"server":"rail","desired":"running","updated_at":"2026-11-05T00:00:00Z","exit_intents":[{"operation":"stop","phase":"prepared","prepared_at":"2026-11-05T00:00:00Z"}]}`,
		"accepted no time": `{"version":1,"server":"rail","desired":"running","updated_at":"2026-11-05T00:00:00Z","exit_intents":[{"generation":"g","operation":"stop","phase":"accepted","prepared_at":"2026-11-05T00:00:00Z"}]}`,
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			store := newOperationStore(t, "rail")
			if err := os.MkdirAll(filepath.Dir(store.Path()), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(store.Path(), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.Load(); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("err=%v", err)
			}
			if _, _, err := store.Update(context.Background(), func(rec OperationRecord, exists bool) (OperationRecord, bool, error) {
				return rec, false, nil
			}); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("update err=%v", err)
			}
		})
	}
}

func TestFindExitIntentRequiresExactGenerationAndPrefersAccepted(t *testing.T) {
	acceptedAt := time.Date(2026, 11, 5, 1, 0, 0, 0, time.UTC)
	rec := OperationRecord{ExitIntents: []ExitIntent{
		{Generation: "gen-a", Operation: ExitStop, Phase: IntentPrepared, PreparedAt: acceptedAt},
		{Generation: "gen-b", Operation: ExitRestart, Phase: IntentAccepted, PreparedAt: acceptedAt, AcceptedAt: &acceptedAt},
		{Generation: "gen-a", Operation: ExitRestart, Phase: IntentAccepted, PreparedAt: acceptedAt, AcceptedAt: &acceptedAt},
		{Generation: "gen-a", Operation: ExitStop, Phase: IntentPrepared, PreparedAt: acceptedAt.Add(time.Minute)},
	}}
	intent, ok := rec.FindExitIntent("gen-a")
	if !ok || intent.Phase != IntentAccepted || intent.Operation != ExitRestart {
		t.Fatalf("gen-a intent=%+v ok=%v", intent, ok)
	}
	intent, ok = rec.FindExitIntent("gen-b")
	if !ok || intent.Phase != IntentAccepted || intent.Operation != ExitRestart {
		t.Fatalf("gen-b intent=%+v ok=%v", intent, ok)
	}
	if _, ok := rec.FindExitIntent("gen-c"); ok {
		t.Fatal("matched a generation that is not present")
	}
	if _, ok := rec.FindExitIntent("GEN-A"); ok {
		t.Fatal("generation comparison was not exact")
	}
}

func TestFindExitIntentReturnsPreparedWhenNotAccepted(t *testing.T) {
	now := time.Now().UTC()
	rec := OperationRecord{ExitIntents: []ExitIntent{
		{Generation: "gen-a", Operation: ExitStop, Phase: IntentPrepared, PreparedAt: now},
		{Generation: "gen-b", Operation: ExitStop, Phase: IntentAccepted, PreparedAt: now, AcceptedAt: &now},
	}}
	intent, ok := rec.FindExitIntent("gen-a")
	if !ok || intent.Phase != IntentPrepared || intent.Operation != ExitStop {
		t.Fatalf("intent=%+v ok=%v", intent, ok)
	}
}
