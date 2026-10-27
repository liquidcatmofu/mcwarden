//go:build linux

package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	hostlinux "github.com/liquidcatmofu/mcwarden/internal/host/linux"
)

func newStore(t *testing.T, server string) *Store {
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
	store, err := Open(server)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return store
}

func selfIdentity(t *testing.T) Identity {
	t.Helper()
	stat, err := hostlinux.Read(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	return Identity{PID: stat.PID, PGID: stat.PGID, StartTime: stat.StartTime}
}

func TestSaveCreatesPrivateDirectory(t *testing.T) {
	store := newStore(t, "rail")
	if err := store.Save("direct", selfIdentity(t)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Dir(store.Path()))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode=%04o want 0700", info.Mode().Perm())
	}
	if !strings.HasSuffix(store.Path(), "rail.json") {
		t.Fatalf("path=%s", store.Path())
	}
}

func TestLockRejectsUnsafeRuntimeDir(t *testing.T) {
	store := newStore(t, "rail")
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", dir)
	if _, err := store.Lock(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err=%v", err)
	}
}

func TestOpenRejectsRelativeStateDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "relative/path")
	if _, err := Open("rail"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err=%v", err)
	}
}

func TestLockRejectsRelativeRuntimeDir(t *testing.T) {
	store := newStore(t, "rail")
	t.Setenv("XDG_RUNTIME_DIR", "relative/path")
	if _, err := store.Lock(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err=%v", err)
	}
}

func TestSaveInspectManaged(t *testing.T) {
	store := newStore(t, "rail")
	id := selfIdentity(t)
	if err := store.Save("direct", id); err != nil {
		t.Fatalf("save: %v", err)
	}
	info, err := os.Stat(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode=%04o want 0600", info.Mode().Perm())
	}
	inspection, err := store.Inspect()
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if inspection.Status != Managed || inspection.Record == nil {
		t.Fatalf("inspection=%+v", inspection)
	}
	rec := inspection.Record
	if rec.PID != id.PID || rec.PGID != id.PGID || rec.StartTime != id.StartTime || rec.BootID == "" {
		t.Fatalf("record=%+v", rec)
	}
	if rec.Server != "rail" || rec.Runtime != "direct" || rec.Version != 1 {
		t.Fatalf("record=%+v", rec)
	}
}

func TestSaveRejectsIncompleteIdentity(t *testing.T) {
	store := newStore(t, "rail")
	if err := store.Save("direct", Identity{PID: 1, PGID: 1}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err=%v", err)
	}
}

func TestInspectAbsent(t *testing.T) {
	store := newStore(t, "rail")
	inspection, err := store.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Status != Absent || inspection.Record != nil {
		t.Fatalf("inspection=%+v", inspection)
	}
}

func TestInspectStaleWhenGone(t *testing.T) {
	store := newStore(t, "rail")
	if err := store.Save("direct", Identity{PID: 1 << 30, PGID: 1 << 30, StartTime: 1}); err != nil {
		t.Fatal(err)
	}
	inspection, err := store.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Status != Stale {
		t.Fatalf("inspection=%+v", inspection)
	}
}

func TestInspectStaleOnBootChange(t *testing.T) {
	store := newStore(t, "rail")
	if err := store.Save("direct", selfIdentity(t)); err != nil {
		t.Fatal(err)
	}
	original := bootID
	bootID = func() (string, error) { return "other-boot", nil }
	t.Cleanup(func() { bootID = original })

	inspection, err := store.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Status != Stale || !strings.Contains(inspection.Reason, "boot id") {
		t.Fatalf("inspection=%+v", inspection)
	}
}

func TestInspectStaleOnStartTimeMismatch(t *testing.T) {
	store := newStore(t, "rail")
	id := selfIdentity(t)
	id.StartTime++
	if err := store.Save("direct", id); err != nil {
		t.Fatal(err)
	}
	inspection, err := store.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Status != Stale || !strings.Contains(inspection.Reason, "start time") {
		t.Fatalf("inspection=%+v", inspection)
	}
}

func TestInspectStaleOnPGIDChange(t *testing.T) {
	store := newStore(t, "rail")
	id := selfIdentity(t)
	id.PGID++
	if err := store.Save("direct", id); err != nil {
		t.Fatal(err)
	}
	inspection, err := store.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Status != Stale || !strings.Contains(inspection.Reason, "process group") {
		t.Fatalf("inspection=%+v", inspection)
	}
}

func TestInspectStaleOnZombie(t *testing.T) {
	store := newStore(t, "rail")
	id := selfIdentity(t)
	if err := store.Save("direct", id); err != nil {
		t.Fatal(err)
	}
	original := procRead
	procRead = func(pid int) (hostlinux.Process, error) {
		return hostlinux.Process{PID: pid, PGID: id.PGID, State: 'Z', StartTime: id.StartTime}, nil
	}
	t.Cleanup(func() { procRead = original })

	inspection, err := store.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Status != Stale || !strings.Contains(inspection.Reason, "exited") {
		t.Fatalf("inspection=%+v", inspection)
	}
}

func TestInspectUnknownOnReadError(t *testing.T) {
	store := newStore(t, "rail")
	if err := store.Save("direct", selfIdentity(t)); err != nil {
		t.Fatal(err)
	}
	original := procRead
	procRead = func(int) (hostlinux.Process, error) { return hostlinux.Process{}, syscall.EACCES }
	t.Cleanup(func() { procRead = original })

	inspection, err := store.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Status != Unknown || !strings.Contains(inspection.Reason, "cannot read") {
		t.Fatalf("inspection=%+v", inspection)
	}
}

func TestInspectUnknownOnBootError(t *testing.T) {
	store := newStore(t, "rail")
	if err := store.Save("direct", selfIdentity(t)); err != nil {
		t.Fatal(err)
	}
	original := bootID
	bootID = func() (string, error) { return "", errors.New("unavailable") }
	t.Cleanup(func() { bootID = original })

	inspection, err := store.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Status != Unknown || !strings.Contains(inspection.Reason, "boot id") {
		t.Fatalf("inspection=%+v", inspection)
	}
}

func TestInspectCorrupt(t *testing.T) {
	store := newStore(t, "rail")
	if err := os.MkdirAll(filepath.Dir(store.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.Path(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	inspection, err := store.Inspect()
	if !errors.Is(err, ErrCorrupt) || inspection.Status != Unknown {
		t.Fatalf("inspection=%+v err=%v", inspection, err)
	}
}

func TestClean(t *testing.T) {
	store := newStore(t, "rail")
	if err := store.Save("direct", selfIdentity(t)); err != nil {
		t.Fatal(err)
	}
	if err := store.Clean(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state still present: %v", err)
	}
	if err := store.Clean(); err != nil {
		t.Fatalf("clean absent: %v", err)
	}
}

func TestSaveLeavesNoTempFiles(t *testing.T) {
	store := newStore(t, "rail")
	if err := store.Save("direct", selfIdentity(t)); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(store.Path()))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("left temp file %s", entry.Name())
		}
	}
}

func TestLockIsExclusive(t *testing.T) {
	store := newStore(t, "rail")
	lock, err := store.Lock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Lock(); !errors.Is(err, ErrBusy) {
		t.Fatalf("second lock err=%v", err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	again, err := store.Lock()
	if err != nil {
		t.Fatalf("relock: %v", err)
	}
	if err := again.Unlock(); err != nil {
		t.Fatal(err)
	}
	var nilLock *Lock
	if err := nilLock.Unlock(); err != nil {
		t.Fatalf("nil unlock: %v", err)
	}
}

func TestFileKey(t *testing.T) {
	if got := fileKey("rail-1.slot_2"); got != "rail-1.slot_2" {
		t.Fatalf("safe key=%q", got)
	}
	for _, name := range []string{"", ".", "..", "../escape", "a/b", "名前"} {
		key := fileKey(name)
		if strings.ContainsAny(key, `/\`) || key == "." || key == ".." {
			t.Fatalf("fileKey(%q)=%q escapes the directory", name, key)
		}
		if key != fileKey(name) {
			t.Fatalf("fileKey(%q) is not deterministic", name)
		}
	}
	if fileKey("a/b") == fileKey("a\\b") {
		t.Fatal("distinct names collided")
	}
}
