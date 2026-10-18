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

func TestSignalName(t *testing.T) {
	for sig, want := range map[syscall.Signal]string{syscall.SIGSEGV: "SIGSEGV", syscall.SIGABRT: "SIGABRT"} {
		if got := signalName(sig); got != want {
			t.Errorf("signalName(%d)=%q want %q", sig, got, want)
		}
	}
}
