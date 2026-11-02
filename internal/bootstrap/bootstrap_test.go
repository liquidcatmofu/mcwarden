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
	"github.com/liquidcatmofu/mcwarden/internal/minecraft"
	"github.com/liquidcatmofu/mcwarden/internal/minecraft/rcon"
	"github.com/liquidcatmofu/mcwarden/internal/runtime"
	"github.com/liquidcatmofu/mcwarden/internal/runtime/podman"
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

func TestServicePodman(t *testing.T) {
	path := writeConfig(t, `
[servers.rail]
runtime = "podman"
[servers.rail.podman]
unit = "minecraft-rail.service"
container = "minecraft-rail"
[servers.rail.direct]
pidfile = "/must/not/be/read"
[servers.rail.minecraft]
control = "rcon"
[servers.rail.minecraft.rcon]
address = "127.0.0.1:25575"
password_file = "/not/read/until/command"
`)
	// A corrupt Direct identity must not block constructing a Podman service.
	store, err := state.Open("rail")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(store.Path()), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.Path(), []byte("corrupt direct identity"), 0600); err != nil {
		t.Fatal(err)
	}
	service, err := Service(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if service.RuntimeName != "podman" || service.StateStore != nil || service.OperationLock == nil {
		t.Fatalf("service=%+v", service)
	}
	if _, ok := service.Runtime.(*podman.Runtime); !ok {
		t.Fatalf("runtime=%T", service.Runtime)
	}
	if _, ok := service.Runtime.(runtime.Identifiable); ok {
		t.Fatal("Podman exposes host process identity")
	}
	if _, ok := service.Minecraft.(*rcon.Client); !ok {
		t.Fatalf("control=%T", service.Minecraft)
	}
	ctx := context.Background()
	if _, err := service.StateStatus(ctx); app.ExitCode(err) != 5 {
		t.Fatalf("state show=%v", err)
	}
	if _, err := service.CleanState(ctx); app.ExitCode(err) != 5 {
		t.Fatalf("state clean=%v", err)
	}
	// Podman must not depend on XDG_STATE_HOME, even for its lifecycle lock.
	t.Setenv("XDG_STATE_HOME", "relative/invalid")
	if _, err := Service(path, ""); err != nil {
		t.Fatal(err)
	}
	runDir := t.TempDir()
	if err := os.Chmod(runDir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", runDir)
	lock, err := service.OperationLock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	// Lifecycle must fail at the lock before ever invoking a host CLI.
	if _, err := service.Start(ctx, app.StartOptions{NoWait: true}); !errors.Is(err, state.ErrBusy) {
		t.Fatalf("start lock=%v", err)
	}
	if _, err := service.Stop(ctx, app.StopOptions{Timeout: 1}); !errors.Is(err, state.ErrBusy) {
		t.Fatalf("stop lock=%v", err)
	}
	if _, err := service.Restart(ctx, app.RestartOptions{NoWait: true, StopTimeout: 1}); !errors.Is(err, state.ErrBusy) {
		t.Fatalf("restart lock=%v", err)
	}
	// Readiness remains unsupported and is rejected before any mutation.
	if _, err := service.Start(ctx, app.StartOptions{StartTimeout: 1}); app.ExitCode(err) != 5 {
		t.Fatalf("start readiness=%v", err)
	}
	if _, err := service.Restart(ctx, app.RestartOptions{StartTimeout: 1, StopTimeout: 1}); app.ExitCode(err) != 5 {
		t.Fatalf("restart readiness=%v", err)
	}
}

func TestServiceWiresRCONControl(t *testing.T) {
	passwordFile := filepath.Join(t.TempDir(), "rcon-password")
	if err := os.WriteFile(passwordFile, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, fmt.Sprintf(`
[servers.rail.direct]
command = "/bin/true"

[servers.rail.minecraft]
control = "rcon"

[servers.rail.minecraft.rcon]
password_file = %q
`, passwordFile))
	service, err := Service(path, "")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if service.Minecraft == nil {
		t.Fatal("minecraft control was not wired")
	}
	if _, ok := service.Minecraft.(*rcon.Client); !ok {
		t.Fatalf("minecraft control=%T, want *rcon.Client", service.Minecraft)
	}
	caps, err := service.Minecraft.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps != (minecraft.Capabilities{Command: true, Readiness: false}) {
		t.Fatalf("capabilities=%+v", caps)
	}
}

func TestServiceWithoutMinecraftControl(t *testing.T) {
	path := writeConfig(t, "[servers.rail.direct]\ncommand = \"/bin/true\"\n")
	service, err := Service(path, "")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if service.Minecraft != nil {
		t.Fatalf("minecraft control=%T, want nil", service.Minecraft)
	}
}

func TestServiceDefersRCONCredentialErrors(t *testing.T) {
	for name, content := range map[string]string{
		"empty":        "\n",
		"empty crlf":   "\r\n",
		"missing file": "",
	} {
		t.Run(name, func(t *testing.T) {
			passwordFile := filepath.Join(t.TempDir(), "rcon-password")
			if name != "missing file" {
				if err := os.WriteFile(passwordFile, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			path := writeConfig(t, fmt.Sprintf(`
[servers.rail.direct]
command = "/bin/true"

[servers.rail.minecraft]
control = "rcon"

[servers.rail.minecraft.rcon]
password_file = %q
`, passwordFile))
			service, err := Service(path, "")
			if err != nil {
				t.Fatalf("bootstrap must not read credentials: %v", err)
			}
			if _, err := service.Status(context.Background()); err != nil {
				t.Fatalf("status must work without credentials: %v", err)
			}
			if _, err := service.StateStatus(context.Background()); err != nil {
				t.Fatalf("state inspection must work without credentials: %v", err)
			}
			err = service.Minecraft.Command(context.Background(), "stop")
			if name == "missing file" {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("err=%v want missing password file", err)
				}
			} else if !errors.Is(err, rcon.ErrEmptyPassword) {
				t.Fatalf("err=%v want empty password", err)
			}
		})
	}
}

func TestServiceStartsWithoutRCONCredentials(t *testing.T) {
	if _, err := os.Stat("/bin/sleep"); err != nil {
		t.Skip("/bin/sleep is required")
	}
	passwordFile := filepath.Join(t.TempDir(), "absent")
	path := writeConfig(t, fmt.Sprintf(`
[servers.rail.direct]
command = "/bin/sleep"
args = ["60"]

[servers.rail.minecraft]
control = "rcon"

[servers.rail.minecraft.rcon]
password_file = %q
`, passwordFile))
	service, err := Service(path, "")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := service.Start(context.Background(), app.StartOptions{NoWait: true}); err != nil {
		t.Fatalf("start --no-wait must not need RCON credentials: %v", err)
	}
	t.Cleanup(func() {
		_ = service.Runtime.Signal(context.Background(), runtime.Kill)
		_, _ = service.Runtime.Wait(context.Background())
	})
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
		"podman":         {writeConfig(t, "[servers.rail]\nruntime = \"podman\"\n"), "", app.ErrConfig},
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
