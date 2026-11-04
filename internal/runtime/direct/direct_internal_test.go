//go:build linux

package direct

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"

	hostlinux "github.com/liquidcatmofu/mcwarden/internal/host/linux"
	"github.com/liquidcatmofu/mcwarden/internal/runtime"
)

// TestAdoptReadErrorIsNotGone verifies that a procfs failure other than ENOENT
// is surfaced instead of being treated as a stopped process, which would let a
// restart launch a second process while the first is still alive.
func TestAdoptReadErrorIsNotGone(t *testing.T) {
	original := procRead
	procRead = func(int) (hostlinux.Process, error) {
		return hostlinux.Process{}, errors.New("procfs unavailable")
	}
	t.Cleanup(func() { procRead = original })

	rt := New(Config{Adopt: &Adopt{PID: os.Getpid(), StartTime: 1}})
	if _, err := rt.State(context.Background()); err == nil {
		t.Fatal("State reported no error for an unreadable process")
	}
	if _, err := rt.Wait(context.Background()); err == nil {
		t.Fatal("Wait reported no error for an unreadable process")
	}
	if err := rt.Signal(context.Background(), runtime.Terminate); err == nil {
		t.Fatal("Signal reported no error for an unreadable process")
	}
}

// TestAdoptESRCHIsGone verifies that a procfs read that fails with ESRCH while
// a process exits is treated as gone, not as an unreadable process. Before this
// was handled, the wait loop could surface "no such process" as an error.
func TestAdoptESRCHIsGone(t *testing.T) {
	original := procRead
	calls := 0
	procRead = func(int) (hostlinux.Process, error) {
		calls++
		if calls <= 2 {
			return hostlinux.Process{PID: os.Getpid(), PGID: os.Getpid(), State: 'S', StartTime: 1}, nil
		}
		return hostlinux.Process{}, &os.PathError{Op: "read", Path: "/proc/1/stat", Err: syscall.ESRCH}
	}
	t.Cleanup(func() { procRead = original })

	rt := New(Config{Adopt: &Adopt{PID: os.Getpid(), StartTime: 1}})
	if state, err := rt.State(context.Background()); err != nil || state != runtime.Running {
		t.Fatalf("state=%s err=%v", state, err)
	}
	exit, err := rt.Wait(context.Background())
	if err != nil || exit.Known {
		t.Fatalf("wait exit=%+v err=%v", exit, err)
	}
	if err := rt.Signal(context.Background(), runtime.Terminate); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("signal err=%v", err)
	}
	// A read that fails before any process is pinned means not running.
	rt = New(Config{Adopt: &Adopt{PID: os.Getpid(), StartTime: 1}})
	if _, err := rt.Wait(context.Background()); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("wait before resolve err=%v", err)
	}
}

func TestSignalName(t *testing.T) {
	for sig, want := range map[syscall.Signal]string{syscall.SIGSEGV: "SIGSEGV", syscall.SIGABRT: "SIGABRT"} {
		if got := signalName(sig); got != want {
			t.Errorf("signalName(%d)=%q want %q", sig, got, want)
		}
	}
}

func TestGenerationKey(t *testing.T) {
	original := readBootID
	t.Cleanup(func() { readBootID = original })
	boot := "boot-a"
	readBootID = func() (string, error) { return boot, nil }

	generation := func(pid int, start uint64, running bool) (string, bool, error) {
		p := &process{pid: pid, startTime: start}
		if running {
			p.done = make(chan struct{})
		} else {
			p.done = make(chan struct{})
			close(p.done)
		}
		return (&Runtime{proc: p}).Generation(context.Background())
	}

	first, ok, err := generation(123, 99, true)
	if err != nil || !ok || first == "" {
		t.Fatalf("generation ok=%v err=%v key=%q", ok, err, first)
	}
	same, ok, err := generation(123, 99, true)
	if err != nil || !ok || same != first {
		t.Fatalf("same identity changed key: %q %q err=%v", first, same, err)
	}
	boot = "boot-b"
	otherBoot, ok, err := generation(123, 99, true)
	if err != nil || !ok || otherBoot == first {
		t.Fatalf("boot id change reused key: %q %q err=%v", first, otherBoot, err)
	}
	boot = "boot-a"
	otherStart, ok, err := generation(123, 100, true)
	if err != nil || !ok || otherStart == first {
		t.Fatalf("start time change reused key: %q %q err=%v", first, otherStart, err)
	}
	otherPID, ok, err := generation(124, 99, true)
	if err != nil || !ok || otherPID == first {
		t.Fatalf("pid change reused key: %q %q err=%v", first, otherPID, err)
	}
	if _, ok, err := generation(123, 99, false); ok || err != nil {
		t.Fatalf("exited generation ok=%v err=%v", ok, err)
	}
	if _, ok, err := (&Runtime{}).Generation(context.Background()); ok || err != nil {
		t.Fatalf("untracked generation ok=%v err=%v", ok, err)
	}
}
