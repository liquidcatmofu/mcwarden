//go:build linux

package state

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"
)

func newReadinessStore(t *testing.T, server string) *ReadinessStore {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", dir)
	store, err := OpenReadiness(server)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestReadinessUpdateSerializesWriters(t *testing.T) {
	store := newReadinessStore(t, "rail")
	other, err := OpenReadiness("rail")
	if err != nil {
		t.Fatal(err)
	}
	stores := []*ReadinessStore{store, other}

	if _, _, err := store.Update(context.Background(), func(ReadinessRecord, bool) (ReadinessRecord, bool, error) {
		return ReadinessRecord{Generation: "gen", Log: &LogBoundary{Offset: 0}}, true, nil
	}); err != nil {
		t.Fatal(err)
	}

	const writers = 8
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(store *ReadinessStore) {
			defer wg.Done()
			_, _, err := store.Update(context.Background(), func(rec ReadinessRecord, exists bool) (ReadinessRecord, bool, error) {
				if !exists {
					t.Error("record disappeared during update")
					return ReadinessRecord{}, false, nil
				}
				// Hold the lock long enough that an unserialized read-modify-write
				// would lose increments.
				offset := rec.Log.Offset
				time.Sleep(time.Millisecond)
				rec.Log.Offset = offset + 1
				return rec, true, nil
			})
			if err != nil {
				t.Errorf("update: %v", err)
			}
		}(stores[i%len(stores)])
	}
	wg.Wait()

	rec, ok, err := store.Load()
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	if rec.Log == nil || rec.Log.Offset != writers {
		t.Fatalf("offset=%v want %d (lost update)", rec.Log, writers)
	}
}

func TestReadinessUpdateWithoutWriteLeavesRecord(t *testing.T) {
	store := newReadinessStore(t, "rail")
	if _, _, err := store.Update(context.Background(), func(ReadinessRecord, bool) (ReadinessRecord, bool, error) {
		return ReadinessRecord{Generation: "gen-a", Ready: true}, true, nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := store.Update(context.Background(), func(rec ReadinessRecord, exists bool) (ReadinessRecord, bool, error) {
		if !exists || rec.Generation != "gen-a" {
			t.Fatalf("unexpected current record: %+v exists=%v", rec, exists)
		}
		return ReadinessRecord{Generation: "gen-b"}, false, nil
	}); err != nil {
		t.Fatal(err)
	}

	rec, ok, err := store.Load()
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	if rec.Generation != "gen-a" || !rec.Ready {
		t.Fatalf("record overwritten: %+v", rec)
	}
}
