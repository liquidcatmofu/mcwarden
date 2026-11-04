//go:build linux

package log

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/liquidcatmofu/mcwarden/internal/minecraft"
	"github.com/liquidcatmofu/mcwarden/internal/state"
)

const doneLine = "[12:00:00] [Server thread/INFO]: Done (3.958s)! For help, type \"help\"\n"
const forgeDoneLine = "[12:00:00] [Server thread/INFO] [minecraft/MinecraftServer]: Done (3.958s)! For help, type \"help\"\n"

func isolateState(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", dir)
}

func newObserver(t *testing.T) (*Observer, *state.ReadinessStore, string) {
	t.Helper()
	isolateState(t)
	path := filepath.Join(t.TempDir(), "latest.log")
	store, err := state.OpenReadiness("rail")
	if err != nil {
		t.Fatal(err)
	}
	return New(path, store), store, path
}

func appendFile(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func bind(t *testing.T, o *Observer, generation string) {
	t.Helper()
	if err := o.Prepare(context.Background()); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := o.Bind(context.Background(), generation); err != nil {
		t.Fatalf("bind: %v", err)
	}
}

func requireState(t *testing.T, o *Observer, generation string, want minecraft.State) {
	t.Helper()
	got, err := o.State(context.Background(), generation)
	if err != nil || got != want {
		t.Fatalf("state=%s err=%v want=%s", got, err, want)
	}
}

func TestPrepareIgnoresOldDone(t *testing.T) {
	o, _, path := newObserver(t)
	writeFile(t, path, doneLine+"[12:00:01] [Server thread/INFO]: old server still running\n")
	bind(t, o, "gen-a")
	requireState(t, o, "gen-a", minecraft.Starting)
	appendFile(t, path, "[12:00:02] [Server thread/INFO]: preparing spawn area\n")
	requireState(t, o, "gen-a", minecraft.Starting)
	appendFile(t, path, forgeDoneLine)
	requireState(t, o, "gen-a", minecraft.Running)
}

func TestTruncateSameInode(t *testing.T) {
	o, _, path := newObserver(t)
	writeFile(t, path, doneLine+"[12:00:01] [Server thread/INFO]: old output that makes the file longer\n[12:00:02] [Server thread/INFO]: more old output\n")
	bind(t, o, "gen-a")
	requireState(t, o, "gen-a", minecraft.Starting)
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	appendFile(t, path, doneLine)
	requireState(t, o, "gen-a", minecraft.Running)
}

func TestInodeReplacement(t *testing.T) {
	o, _, path := newObserver(t)
	writeFile(t, path, doneLine)
	bind(t, o, "gen-a")
	requireState(t, o, "gen-a", minecraft.Starting)
	replacement := filepath.Join(t.TempDir(), "new.log")
	writeFile(t, replacement, forgeDoneLine)
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	requireState(t, o, "gen-a", minecraft.Running)
}

func TestAbsentLogThenCreated(t *testing.T) {
	o, _, path := newObserver(t)
	if err := o.Prepare(context.Background()); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := o.Bind(context.Background(), "gen-a"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	requireState(t, o, "gen-a", minecraft.Starting)
	writeFile(t, path, doneLine)
	requireState(t, o, "gen-a", minecraft.Running)
}

func TestEvidenceStates(t *testing.T) {
	o, _, path := newObserver(t)
	requireState(t, o, "gen-a", minecraft.Unknown)
	writeFile(t, path, "[12:00:00] [Server thread/INFO]: starting\n")
	bind(t, o, "gen-a")
	requireState(t, o, "gen-a", minecraft.Starting)
	requireState(t, o, "gen-b", minecraft.Unknown)
	appendFile(t, path, doneLine)
	requireState(t, o, "gen-a", minecraft.Running)
}

func TestCorruptEvidenceFailsClosed(t *testing.T) {
	o, store, _ := newObserver(t)
	if err := os.MkdirAll(filepath.Dir(store.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, store.Path(), "{not json")
	if _, err := o.State(context.Background(), "gen-a"); !errors.Is(err, state.ErrCorrupt) {
		t.Fatalf("err=%v", err)
	}
}

func TestReadySurvivesLogReplacement(t *testing.T) {
	o, _, path := newObserver(t)
	writeFile(t, path, "[12:00:00] [Server thread/INFO]: starting\n")
	bind(t, o, "gen-a")
	appendFile(t, path, doneLine)
	requireState(t, o, "gen-a", minecraft.Running)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, "[12:00:01] [Server thread/INFO]: a new file without the marker\n")
	requireState(t, o, "gen-a", minecraft.Running)
}

func TestPersistenceAcrossObserverInstances(t *testing.T) {
	o, store, path := newObserver(t)
	writeFile(t, path, "[12:00:00] [Server thread/INFO]: starting\n")
	bind(t, o, "gen-a")
	appendFile(t, path, doneLine)
	requireState(t, o, "gen-a", minecraft.Running)

	next := New(path, store)
	requireState(t, next, "gen-a", minecraft.Running)
	requireState(t, next, "gen-b", minecraft.Unknown)
}

func TestMarkerRequiresBothParts(t *testing.T) {
	o, _, path := newObserver(t)
	writeFile(t, path, "")
	bind(t, o, "gen-a")
	appendFile(t, path, "[12:00:00] [Server thread/INFO]: Done (3.958s)!\n")
	requireState(t, o, "gen-a", minecraft.Starting)
	appendFile(t, path, "[12:00:01] [Server thread/INFO]: For help, type \"help\"\n")
	requireState(t, o, "gen-a", minecraft.Starting)
	appendFile(t, path, doneLine)
	requireState(t, o, "gen-a", minecraft.Running)
}

func TestBindRequiresPrepare(t *testing.T) {
	o, _, _ := newObserver(t)
	if err := o.Bind(context.Background(), "gen-a"); !errors.Is(err, ErrNotPrepared) {
		t.Fatalf("err=%v", err)
	}
}

func TestPendingEvidencePromotesAcrossInstances(t *testing.T) {
	o, store, path := newObserver(t)
	writeFile(t, path, doneLine+"[12:00:01] [Server thread/INFO]: old generation output\n")
	bind(t, o, "gen-a")

	next := New(path, store)
	requireState(t, next, "gen-a", minecraft.Starting)
	appendFile(t, path, forgeDoneLine)
	requireState(t, next, "gen-a", minecraft.Running)
}
