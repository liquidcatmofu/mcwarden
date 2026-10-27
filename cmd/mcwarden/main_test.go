//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	hostlinux "github.com/liquidcatmofu/mcwarden/internal/host/linux"
)

// TestCrossInvocationLifecycle builds the CLI and drives one process across
// separate invocations: start, status, failed stop/restart because Minecraft
// control is not wired, external exit, stale-state discovery, and clean.
func TestCrossInvocationLifecycle(t *testing.T) {
	if _, err := os.Stat("/bin/sleep"); err != nil {
		t.Skip("/bin/sleep is required")
	}
	bin := filepath.Join(t.TempDir(), "mcwarden")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build CLI: %v", err)
	}

	stateHome := privateDir(t)
	runtimeHome := privateDir(t)
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(configPath, []byte(`
[servers.rail.direct]
command = "/bin/sleep"
args = ["60"]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "XDG_STATE_HOME="+stateHome, "XDG_RUNTIME_DIR="+runtimeHome)

	run := func(args ...string) (int, []byte, string) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = env
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		code := 0
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return code, stdout.Bytes(), stderr.String()
	}

	code, stdout, stderr := run("start", "--no-wait", "-c", configPath)
	if code != 0 {
		t.Fatalf("start code=%d stderr=%s", code, stderr)
	}
	if !bytes.Contains(stdout, []byte("process_started")) {
		t.Fatalf("start stdout=%s", stdout)
	}

	// Read the persisted generation and make sure a failed assertion below
	// cannot leave the server running.
	pid, pgid := showState(t, run, "managed", configPath)
	t.Cleanup(func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	})

	stateFile := filepath.Join(stateHome, "mcwarden", "rail.json")
	lockFile := filepath.Join(runtimeHome, "mcwarden", "rail.lock")
	if _, err := os.Stat(stateFile); err != nil {
		t.Fatalf("identity not in persistent state directory: %v", err)
	}
	if _, err := os.Stat(lockFile); err != nil {
		t.Fatalf("lock not in runtime directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runtimeHome, "mcwarden", "rail.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("identity leaked into runtime directory: %v", err)
	}

	code, stdout, stderr = run("status", "--json", "-c", configPath)
	if code != 0 {
		t.Fatalf("status code=%d stderr=%s", code, stderr)
	}
	if got := runtimeState(t, stdout); got != "running" {
		t.Fatalf("rehydrated runtime state=%q stdout=%s", got, stdout)
	}

	// A second start must refuse the rehydrated generation.
	if code, _, _ = run("start", "--no-wait", "-c", configPath); code != 1 {
		t.Fatalf("second start code=%d want 1", code)
	}

	// Stop and restart cannot proceed without Minecraft control, but they must
	// see the running generation. Exit 3 would mean the runtime was not
	// rehydrated and the server looked stopped.
	if code, _, stderr = run("stop", "-c", configPath); code != 5 {
		t.Fatalf("stop code=%d want 5 stderr=%s", code, stderr)
	}
	if code, _, stderr = run("restart", "--no-wait", "-c", configPath); code != 5 {
		t.Fatalf("restart code=%d want 5 stderr=%s", code, stderr)
	}
	if _, err := hostlinux.Read(pid); err != nil {
		t.Fatalf("server exited before receiving control: %v", err)
	}

	// An exit outside MCWarden leaves stale state for the explicit clean path.
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill server: %v", err)
	}
	waitGone(t, pid)
	showState(t, run, "stale", configPath)

	code, stdout, stderr = run("state", "clean", "--json", "-c", configPath)
	if code != 0 {
		t.Fatalf("state clean code=%d stderr=%s", code, stderr)
	}
	var cleaned struct {
		Data stateOutput `json:"data"`
	}
	if err := json.Unmarshal(stdout, &cleaned); err != nil {
		t.Fatal(err)
	}
	if cleaned.Data.Status != "absent" {
		t.Fatalf("state clean=%s", stdout)
	}
	if _, err := os.Stat(stateFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state file still present: %v", err)
	}

	code, stdout, stderr = run("status", "--json", "-c", configPath)
	if code != 0 {
		t.Fatalf("status after clean code=%d stderr=%s", code, stderr)
	}
	if got := runtimeState(t, stdout); got != "stopped" {
		t.Fatalf("state after clean=%q", got)
	}
}

type stateOutput struct {
	Status string `json:"status"`
	Record *struct {
		PID  int `json:"pid"`
		PGID int `json:"pgid"`
	} `json:"record"`
}

func showState(t *testing.T, run func(...string) (int, []byte, string), want, configPath string) (int, int) {
	t.Helper()
	code, stdout, stderr := run("state", "show", "--json", "-c", configPath)
	if code != 0 {
		t.Fatalf("state show code=%d stderr=%s", code, stderr)
	}
	var result struct {
		Data stateOutput `json:"data"`
	}
	if err := json.Unmarshal(stdout, &result); err != nil {
		t.Fatal(err)
	}
	if result.Data.Status != want {
		t.Fatalf("state=%s want %s: %s", result.Data.Status, want, stdout)
	}
	if result.Data.Record == nil {
		t.Fatalf("state has no record: %s", stdout)
	}
	return result.Data.Record.PID, result.Data.Record.PGID
}

func runtimeState(t *testing.T, stdout []byte) string {
	t.Helper()
	var status struct {
		Data struct {
			Runtime struct {
				State string `json:"state"`
			} `json:"runtime"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout, &status); err != nil {
		t.Fatal(err)
	}
	return status.Data.Runtime.State
}

func privateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := hostlinux.Read(pid); hostlinux.IsGone(err) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process %d did not exit", pid)
}
