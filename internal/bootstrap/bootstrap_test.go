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
	hostlinux "github.com/liquidcatmofu/mcwarden/internal/host/linux"
	"github.com/liquidcatmofu/mcwarden/internal/runtime"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
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
