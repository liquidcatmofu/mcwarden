//go:build linux

package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"github.com/liquidcatmofu/mcwarden/internal/app"
	"github.com/liquidcatmofu/mcwarden/internal/config"
	hostlinux "github.com/liquidcatmofu/mcwarden/internal/host/linux"
	"github.com/liquidcatmofu/mcwarden/internal/runtime"
	"github.com/liquidcatmofu/mcwarden/internal/state"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	isolateState(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// isolateState keeps persisted identity out of the developer's real state
// directory.
func isolateState(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", dir)
}

func statusOf(t *testing.T, configPath, server string) runtime.State {
	t.Helper()
	service, err := Service(configPath, server)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	status, err := service.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	return status.Runtime.State
}

func TestServiceBuildsDirectRuntime(t *testing.T) {
	path := writeConfig(t, `
[servers.rail.direct]
command = "/bin/true"
dir = "/srv/rail"
args = ["--nogui"]
env = ["MC_TEST=1"]
`)
	if state := statusOf(t, path, ""); state != runtime.Stopped {
		t.Fatalf("state=%s", state)
	}
}

func TestServiceAdoptsPidfile(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "mc.pid")
	if err := os.WriteFile(pidfile, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, fmt.Sprintf(`
[servers.rail.direct]
command = "/bin/true"
pidfile = %q
`, pidfile))
	if state := statusOf(t, path, "rail"); state != runtime.Running {
		t.Fatalf("adopted state=%s", state)
	}
}

func TestServiceAdoptsPidfileWithStartTime(t *testing.T) {
	stat, err := hostlinux.Read(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	pidfile := filepath.Join(t.TempDir(), "mc.pid")
	if err := os.WriteFile(pidfile, []byte(fmt.Sprintf("%d %d", os.Getpid(), stat.StartTime)), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, fmt.Sprintf(`
[servers.rail.direct]
command = "/bin/true"
pidfile = %q
`, pidfile))
	if state := statusOf(t, path, ""); state != runtime.Running {
		t.Fatalf("adopted state=%s", state)
	}
}

func TestServiceRejectsStaleStartTime(t *testing.T) {
	stat, err := hostlinux.Read(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	pidfile := filepath.Join(t.TempDir(), "mc.pid")
	if err := os.WriteFile(pidfile, []byte(fmt.Sprintf("%d %d", os.Getpid(), stat.StartTime+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, fmt.Sprintf(`
[servers.rail.direct]
command = "/bin/true"
pidfile = %q
`, pidfile))
	if state := statusOf(t, path, ""); state != runtime.Stopped {
		t.Fatalf("stale pidfile state=%s", state)
	}
}

func TestServiceMissingPidfileIsStopped(t *testing.T) {
	path := writeConfig(t, fmt.Sprintf(`
[servers.rail.direct]
command = "/bin/true"
pidfile = %q
`, filepath.Join(t.TempDir(), "absent.pid")))
	if state := statusOf(t, path, ""); state != runtime.Stopped {
		t.Fatalf("state=%s", state)
	}
}

// TestServiceRehydratesManagedState verifies that a new invocation restores the
// direct runtime from persisted identity even without a pidfile.
func TestServiceRehydratesManagedState(t *testing.T) {
	path := writeConfig(t, "[servers.rail.direct]\ncommand = \"/bin/true\"\n")
	saveSelfState(t, "rail")
	if state := statusOf(t, path, ""); state != runtime.Running {
		t.Fatalf("state=%s", state)
	}
}

// TestServicePrefersManagedStateOverPidfile verifies that usable persisted
// state wins over a contradictory pidfile.
func TestServicePrefersManagedStateOverPidfile(t *testing.T) {
	stat, err := hostlinux.Read(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	pidfile := filepath.Join(t.TempDir(), "mc.pid")
	if err := os.WriteFile(pidfile, []byte(fmt.Sprintf("%d %d", os.Getpid(), stat.StartTime+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, fmt.Sprintf(`
[servers.rail.direct]
command = "/bin/true"
pidfile = %q
`, pidfile))
	saveSelfState(t, "rail")
	if state := statusOf(t, path, ""); state != runtime.Running {
		t.Fatalf("state=%s", state)
	}
}

// TestServiceIgnoresStaleState verifies that stale state does not resurrect a
// runtime; start reports ErrStaleState until the record is cleaned.
func TestServiceIgnoresStaleState(t *testing.T) {
	path := writeConfig(t, "[servers.rail.direct]\ncommand = \"/bin/true\"\n")
	store, err := state.Open("rail")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save("direct", state.Identity{PID: 1 << 30, PGID: 1 << 30, StartTime: 1}); err != nil {
		t.Fatal(err)
	}
	if state := statusOf(t, path, ""); state != runtime.Stopped {
		t.Fatalf("state=%s", state)
	}
}

// TestServiceStaleStateSkipsPidfile verifies that stale internal state blocks
// operations instead of resurrecting a process through the pidfile.
func TestServiceStaleStateSkipsPidfile(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "mc.pid")
	if err := os.WriteFile(pidfile, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, fmt.Sprintf(`
[servers.rail.direct]
command = "/bin/true"
pidfile = %q
`, pidfile))
	store, err := state.Open("rail")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save("direct", state.Identity{PID: 1 << 30, PGID: 1 << 30, StartTime: 1}); err != nil {
		t.Fatal(err)
	}
	if state := statusOf(t, path, ""); state != runtime.Stopped {
		t.Fatalf("pidfile resurrected stale state: %s", state)
	}
}

// TestRuntimeForUnknownStateIsNotOperable verifies that unverified identity is
// reported as unknown but never adopted, started, or signalled.
func TestRuntimeForUnknownStateIsNotOperable(t *testing.T) {
	stat, err := hostlinux.Read(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	rt, err := runtimeFor("rail", config.Server{Direct: &config.Direct{Command: "/bin/true"}}, state.Inspection{
		Status: state.Unknown,
		Reason: "cannot read boot id",
		Record: &state.Record{PID: stat.PID, PGID: stat.PGID, StartTime: stat.StartTime},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rt.(unknownRuntime); !ok {
		t.Fatalf("runtime=%T, want unknownRuntime", rt)
	}
	ctx := context.Background()
	if got, err := rt.State(ctx); err != nil || got != runtime.Unknown {
		t.Fatalf("state=%s err=%v", got, err)
	}
	if err := rt.Start(ctx); !errors.Is(err, state.ErrUnknown) {
		t.Fatalf("start err=%v", err)
	}
	if err := rt.Signal(ctx, runtime.Kill); !errors.Is(err, state.ErrUnknown) {
		t.Fatalf("signal err=%v", err)
	}
	if _, err := rt.Wait(ctx); !errors.Is(err, state.ErrUnknown) {
		t.Fatalf("wait err=%v", err)
	}
}

func saveSelfState(t *testing.T, server string) {
	t.Helper()
	store, err := state.Open(server)
	if err != nil {
		t.Fatal(err)
	}
	stat, err := hostlinux.Read(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save("direct", state.Identity{
		PID: stat.PID, PGID: stat.PGID, StartTime: stat.StartTime, Owned: true, SignalGroup: true,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestServiceErrors(t *testing.T) {
	valid := writeConfig(t, "[servers.rail.direct]\ncommand = \"/bin/true\"\n")
	for name, tt := range map[string]struct {
		path   string
		server string
		want   error
	}{
		"unknown server": {valid, "missing", app.ErrConfig},
		"missing config": {filepath.Join(t.TempDir(), "absent.toml"), "", app.ErrConfig},
		"podman":         {writeConfig(t, "[servers.rail]\nruntime = \"podman\"\n"), "", app.ErrUnsupported},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Service(tt.path, tt.server); !errors.Is(err, tt.want) {
				t.Fatalf("err=%v want %v", err, tt.want)
			}
		})
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for input, want := range map[string]string{
		"~/minecraft": filepath.Join(home, "minecraft"),
		"~":           home,
		"/abs/path":   "/abs/path",
		"~other/x":    "~other/x",
		"":            "",
	} {
		if got := expandHome(input); got != want {
			t.Errorf("expandHome(%q)=%q want %q", input, got, want)
		}
	}
}

func TestMergeEnv(t *testing.T) {
	if got := mergeEnv([]string{"A=1"}, nil); got != nil {
		t.Fatalf("empty extra should inherit, got %v", got)
	}
	got := mergeEnv([]string{"A=1", "B=2"}, []string{"B=3", "C=4"})
	want := []string{"A=1", "B=3", "C=4"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}
