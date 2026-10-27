//go:build linux

package direct_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	hostlinux "github.com/liquidcatmofu/mcwarden/internal/host/linux"
	"github.com/liquidcatmofu/mcwarden/internal/runtime"
	"github.com/liquidcatmofu/mcwarden/internal/runtime/direct"
)

// TestHelperProcess is not a real test. It acts as a controllable child process
// when re-executed with MCWARDEN_DIRECT_HELPER=1.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("MCWARDEN_DIRECT_HELPER") != "1" {
		return
	}
	switch os.Getenv("MCWARDEN_DIRECT_MODE") {
	case "exit":
		code, _ := strconv.Atoi(os.Getenv("MCWARDEN_DIRECT_CODE"))
		os.Exit(code)
	case "sleep":
		time.Sleep(time.Hour)
	case "ignore-term":
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM)
		writePidFile()
		for range ch {
		}
	case "spawn":
		child := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
		child.Env = append(os.Environ(), "MCWARDEN_DIRECT_HELPER=1", "MCWARDEN_DIRECT_MODE=sleep")
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		if path := os.Getenv("MCWARDEN_DIRECT_PIDFILE"); path != "" {
			_ = os.WriteFile(path, []byte(strconv.Itoa(child.Process.Pid)), 0o644)
		}
		time.Sleep(time.Hour)
	case "regroup":
		writePidFile()
		time.Sleep(time.Second)
		if err := syscall.Setpgid(0, 0); err != nil {
			os.Exit(2)
		}
		time.Sleep(time.Hour)
	}
	os.Exit(0)
}

// writePidFile announces readiness by recording this process's PID.
func writePidFile() {
	if path := os.Getenv("MCWARDEN_DIRECT_PIDFILE"); path != "" {
		_ = os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644)
	}
}

func helperConfig(mode string, extra ...string) direct.Config {
	env := append(os.Environ(), "MCWARDEN_DIRECT_HELPER=1", "MCWARDEN_DIRECT_MODE="+mode)
	env = append(env, extra...)
	return direct.Config{Path: os.Args[0], Args: []string{"-test.run=TestHelperProcess"}, Env: env}
}

// startOwned starts an owned process and registers cleanup that stops it.
func startOwned(t *testing.T, cfg direct.Config) *direct.Runtime {
	t.Helper()
	rt := direct.New(cfg)
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		rt.Signal(context.Background(), runtime.Kill)
		rt.Wait(context.Background())
	})
	return rt
}

func TestStartWaitExitCode(t *testing.T) {
	rt := direct.New(helperConfig("exit", "MCWARDEN_DIRECT_CODE=7"))
	ctx := context.Background()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	exit, err := rt.Wait(ctx)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if !exit.Known || exit.Signal != nil || exit.Code != 7 {
		t.Fatalf("exit=%+v", exit)
	}
	if state, err := rt.State(ctx); err != nil || state != runtime.Stopped {
		t.Fatalf("state=%s err=%v", state, err)
	}
}

func TestWaitBeforeStart(t *testing.T) {
	rt := direct.New(helperConfig("sleep"))
	if _, err := rt.Wait(context.Background()); !errors.Is(err, direct.ErrNotStarted) {
		t.Fatalf("err=%v", err)
	}
}

func TestStartRefusesWhileRunning(t *testing.T) {
	rt := startOwned(t, helperConfig("sleep"))
	if err := rt.Start(context.Background()); !errors.Is(err, direct.ErrAlreadyRunning) {
		t.Fatalf("err=%v", err)
	}
}

func TestStartAfterExit(t *testing.T) {
	rt := direct.New(helperConfig("exit", "MCWARDEN_DIRECT_CODE=0"))
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := rt.Start(ctx); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		if _, err := rt.Wait(ctx); err != nil {
			t.Fatalf("wait %d: %v", i, err)
		}
	}
}

func TestWaitRetryAfterTimeout(t *testing.T) {
	rt := startOwned(t, helperConfig("sleep"))
	ctx := context.Background()

	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := rt.Wait(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout wait err=%v", err)
	}
	if err := rt.Signal(ctx, runtime.Terminate); err != nil {
		t.Fatalf("signal: %v", err)
	}
	exit, err := rt.Wait(ctx)
	if err != nil {
		t.Fatalf("retry wait: %v", err)
	}
	if !exit.Known || exit.Signal == nil || exit.Signal.Name != "SIGTERM" || exit.Signal.Number != int(syscall.SIGTERM) || exit.CoreDumped {
		t.Fatalf("exit=%+v", exit)
	}
}

func TestWaitHonorsCancellation(t *testing.T) {
	rt := startOwned(t, helperConfig("sleep"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := rt.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	// Cancellation must not have stopped the process.
	if state, err := rt.State(context.Background()); err != nil || state != runtime.Running {
		t.Fatalf("state=%s err=%v", state, err)
	}
}

func TestTerminateThenKill(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "ready.pid")
	rt := startOwned(t, helperConfig("ignore-term", "MCWARDEN_DIRECT_PIDFILE="+pidfile))
	ctx := context.Background()
	waitForPid(t, pidfile) // the SIGTERM handler is installed before this

	if err := rt.Signal(ctx, runtime.Terminate); err != nil {
		t.Fatalf("term: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if state, err := rt.State(ctx); err != nil || state != runtime.Running {
		t.Fatalf("state after ignored SIGTERM=%s err=%v", state, err)
	}
	if err := rt.Signal(ctx, runtime.Kill); err != nil {
		t.Fatalf("kill: %v", err)
	}
	exit, err := rt.Wait(ctx)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if !exit.Known || exit.Signal == nil || exit.Signal.Name != "SIGKILL" || exit.Signal.Number != int(syscall.SIGKILL) {
		t.Fatalf("exit=%+v", exit)
	}
}

func TestUnsupportedSignal(t *testing.T) {
	rt := startOwned(t, helperConfig("sleep"))
	if err := rt.Signal(context.Background(), runtime.Signal("hup")); !errors.Is(err, direct.ErrUnsupported) {
		t.Fatalf("err=%v", err)
	}
}

func TestStartHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rt := direct.New(helperConfig("sleep"))
	if err := rt.Start(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if state, err := rt.State(context.Background()); err != nil || state != runtime.Stopped {
		t.Fatalf("state=%s err=%v", state, err)
	}
}

// TestOwnedProcessGroupSignal verifies that an owned process is its own group
// leader and that a group signal also reaches its children.
func TestOwnedProcessGroupSignal(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "grandchild.pid")
	rt := startOwned(t, helperConfig("spawn", "MCWARDEN_DIRECT_PIDFILE="+pidfile))
	ctx := context.Background()

	grandchild := waitForPid(t, pidfile)
	gc := readStat(t, grandchild)
	owner := readStat(t, gc.PPID)
	if owner.PGID != owner.PID {
		t.Fatalf("owned process is not a group leader: %+v", owner)
	}
	if gc.PGID != owner.PID {
		t.Fatalf("grandchild pgid=%d want %d", gc.PGID, owner.PID)
	}

	if err := rt.Signal(ctx, runtime.Kill); err != nil {
		t.Fatalf("kill: %v", err)
	}
	if _, err := rt.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}
	waitGone(t, grandchild, 5*time.Second)
}

// TestAdoptRejectsChangedPGID verifies that a recorded process group is checked
// on adoption, so a moved process is neither reported as stopped nor signalled.
func TestAdoptRejectsChangedPGID(t *testing.T) {
	pid, stop := startExternal(t, "sleep", "")
	defer stop()
	stat := readStat(t, pid)
	rt := direct.New(direct.Config{Adopt: &direct.Adopt{
		PID: stat.PID, PGID: stat.PGID + 1, StartTime: stat.StartTime, Owned: true,
	}})
	if _, err := rt.State(context.Background()); !errors.Is(err, direct.ErrIdentityChanged) {
		t.Fatalf("err=%v", err)
	}
	if err := rt.Signal(context.Background(), runtime.Kill); !errors.Is(err, direct.ErrIdentityChanged) {
		t.Fatalf("signal err=%v", err)
	}
	if _, err := hostlinux.Read(pid); err != nil {
		t.Fatalf("process was signalled: %v", err)
	}
}

// TestAdoptOwnedGeneration verifies that persisted MCWarden ownership is
// restored without pretending the process is a waitable child of this process.
func TestAdoptOwnedGeneration(t *testing.T) {
	ctx := context.Background()
	pid, stop := startExternal(t, "sleep", "")
	defer stop()
	stat := readStat(t, pid)
	rt := direct.New(direct.Config{Adopt: &direct.Adopt{
		PID: stat.PID, PGID: stat.PGID, StartTime: stat.StartTime, Owned: true,
	}})

	id, ok := rt.Identity()
	if !ok {
		t.Fatal("no identity")
	}
	if !id.Owned || !id.SignalGroup || id.PID != stat.PID || id.PGID != stat.PGID || id.StartTime != stat.StartTime {
		t.Fatalf("identity=%+v stat=%+v", id, stat)
	}
	if state, err := rt.State(ctx); err != nil || state != runtime.Running {
		t.Fatalf("state=%s err=%v", state, err)
	}
	if err := rt.Start(ctx); !errors.Is(err, direct.ErrAlreadyRunning) {
		t.Fatalf("start while rehydrated err=%v", err)
	}
	if err := rt.Signal(ctx, runtime.Kill); err != nil {
		t.Fatalf("signal: %v", err)
	}
	exit, err := rt.Wait(ctx)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if exit.Known {
		t.Fatalf("rehydrated owned exit should be unknown: %+v", exit)
	}
	if state, err := rt.State(ctx); err != nil || state != runtime.Stopped {
		t.Fatalf("state=%s err=%v", state, err)
	}
}

// TestAdoptOwnedRequiresLeader verifies that restoring ownership never turns a
// process that merely belongs to a group into a group-signalling target.
func TestAdoptOwnedRequiresLeader(t *testing.T) {
	pid, stop := startExternalUngrouped(t, "sleep", "")
	defer stop()
	stat := readStat(t, pid)
	if stat.PGID == stat.PID {
		t.Skip("helper unexpectedly leads its own group")
	}
	rt := direct.New(direct.Config{Adopt: &direct.Adopt{
		PID: stat.PID, PGID: stat.PGID, StartTime: stat.StartTime, Owned: true,
	}})
	if err := rt.Signal(context.Background(), runtime.Kill); !errors.Is(err, direct.ErrIdentityChanged) {
		t.Fatalf("err=%v", err)
	}
	if _, err := hostlinux.Read(pid); err != nil {
		t.Fatalf("process was signalled: %v", err)
	}
}

// TestAdoptExistingProcess verifies that a plain adopted process stays
// unowned and waits through procfs.
func TestAdoptExistingProcess(t *testing.T) {
	ctx := context.Background()
	pid, stop := startExternal(t, "sleep", "")
	defer stop()

	stat := readStat(t, pid)
	rt := direct.New(direct.Config{Adopt: &direct.Adopt{PID: stat.PID, StartTime: stat.StartTime}})

	if state, err := rt.State(ctx); err != nil || state != runtime.Running {
		t.Fatalf("state=%s err=%v", state, err)
	}
	if err := rt.Start(ctx); !errors.Is(err, direct.ErrAlreadyRunning) {
		t.Fatalf("start while adopted err=%v", err)
	}
	if err := rt.Signal(ctx, runtime.Kill); err != nil {
		t.Fatalf("signal: %v", err)
	}
	exit, err := rt.Wait(ctx)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if exit.Known {
		t.Fatalf("adopted exit should be unknown: %+v", exit)
	}
	if state, err := rt.State(ctx); err != nil || state != runtime.Stopped {
		t.Fatalf("state=%s err=%v", state, err)
	}
}

// TestAdoptRejectsChangedStartTime verifies that a PID recorded with a stale
// start time is never targeted, protecting against PID reuse.
func TestAdoptRejectsChangedStartTime(t *testing.T) {
	pid, stop := startExternal(t, "sleep", "")
	defer stop()

	stat := readStat(t, pid)
	rt := direct.New(direct.Config{Adopt: &direct.Adopt{PID: stat.PID, StartTime: stat.StartTime + 1}})
	if state, err := rt.State(context.Background()); err != nil || state != runtime.Stopped {
		t.Fatalf("state=%s err=%v", state, err)
	}
	if err := rt.Signal(context.Background(), runtime.Kill); !errors.Is(err, direct.ErrNotStarted) {
		t.Fatalf("err=%v", err)
	}
	if _, err := hostlinux.Read(pid); err != nil {
		t.Fatalf("mismatched adopt signalled another process: %v", err)
	}
}

func TestAdoptRequiresStartTime(t *testing.T) {
	rt := direct.New(direct.Config{Adopt: &direct.Adopt{PID: os.Getpid()}})
	if _, err := rt.State(context.Background()); !errors.Is(err, direct.ErrInvalid) {
		t.Fatalf("err=%v", err)
	}
}

// TestAdoptTreatsZombieAsStopped verifies that an exited but unreaped process is
// reported as stopped rather than running forever.
func TestAdoptTreatsZombieAsStopped(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
	cmd.Env = append(os.Environ(), "MCWARDEN_DIRECT_HELPER=1", "MCWARDEN_DIRECT_MODE=exit", "MCWARDEN_DIRECT_CODE=0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer cmd.Wait()

	deadline := time.Now().Add(5 * time.Second)
	var stat hostlinux.Process
	for {
		got, err := hostlinux.Read(cmd.Process.Pid)
		if err == nil && got.State == 'Z' {
			stat = got
			break
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatalf("process did not become a zombie: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	rt := direct.New(direct.Config{Adopt: &direct.Adopt{PID: stat.PID, StartTime: stat.StartTime}})
	if state, err := rt.State(context.Background()); err != nil || state != runtime.Stopped {
		t.Fatalf("state=%s err=%v", state, err)
	}
}

// TestAdoptGroupSignal verifies the explicit group-signal authorization.
func TestAdoptGroupSignal(t *testing.T) {
	ctx := context.Background()
	pidfile := filepath.Join(t.TempDir(), "grandchild.pid")
	pid, stop := startExternal(t, "spawn", pidfile)
	defer stop()

	grandchild := waitForPid(t, pidfile)
	stat := readStat(t, pid)
	rt := direct.New(direct.Config{Adopt: &direct.Adopt{PID: stat.PID, StartTime: stat.StartTime, SignalGroup: true}})

	if err := rt.Signal(ctx, runtime.Kill); err != nil {
		t.Fatalf("signal: %v", err)
	}
	if _, err := rt.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}
	waitGone(t, grandchild, 5*time.Second)
}

// TestIdentityReportsOwnedGeneration verifies that Start exposes the PID,
// process group, and start time used for persisted state.
func TestIdentityReportsOwnedGeneration(t *testing.T) {
	rt := startOwned(t, helperConfig("sleep"))
	id, ok := rt.Identity()
	if !ok {
		t.Fatal("no identity")
	}
	if !id.Owned || !id.SignalGroup {
		t.Fatalf("identity=%+v", id)
	}
	stat := readStat(t, id.PID)
	if id.PGID != stat.PGID || id.StartTime != stat.StartTime {
		t.Fatalf("identity=%+v stat=%+v", id, stat)
	}
	if id.PID != id.PGID {
		t.Fatalf("owned process is not its group leader: %+v", id)
	}
}

// TestIdentityReportsAdoptedGeneration verifies that an adopted process keeps
// the identity read from procfs.
func TestIdentityReportsAdoptedGeneration(t *testing.T) {
	pid, stop := startExternal(t, "sleep", "")
	defer stop()
	stat := readStat(t, pid)
	rt := direct.New(direct.Config{Adopt: &direct.Adopt{PID: stat.PID, StartTime: stat.StartTime}})

	id, ok := rt.Identity()
	if !ok {
		t.Fatal("no identity")
	}
	if id.Owned || id.PID != stat.PID || id.PGID != stat.PGID || id.StartTime != stat.StartTime {
		t.Fatalf("identity=%+v stat=%+v", id, stat)
	}
}

// TestAdoptGroupSignalRequiresLeader verifies that signal_group=true never
// turns a process that merely belongs to a group into a group handle.
func TestAdoptGroupSignalRequiresLeader(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
	cmd.Env = append(os.Environ(), "MCWARDEN_DIRECT_HELPER=1", "MCWARDEN_DIRECT_MODE=sleep")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start external: %v", err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()

	stat := readStat(t, cmd.Process.Pid)
	if stat.PGID == stat.PID {
		t.Skip("helper unexpectedly leads its own group")
	}
	rt := direct.New(direct.Config{Adopt: &direct.Adopt{PID: stat.PID, StartTime: stat.StartTime, SignalGroup: true}})
	if err := rt.Signal(context.Background(), runtime.Terminate); !errors.Is(err, direct.ErrIdentityChanged) {
		t.Fatalf("err=%v", err)
	}
	if _, err := hostlinux.Read(stat.PID); err != nil {
		t.Fatalf("process was signalled: %v", err)
	}
}

// TestSignalRejectsMovedProcessGroup verifies that a process which changes its
// process group after adoption is not signalled through the stale group id.
func TestSignalRejectsMovedProcessGroup(t *testing.T) {
	ctx := context.Background()
	pid, stop := startExternalUngrouped(t, "regroup", "")
	defer stop()

	stat := readStat(t, pid)
	rt := direct.New(direct.Config{Adopt: &direct.Adopt{PID: stat.PID, StartTime: stat.StartTime, SignalGroup: true}})
	if state, err := rt.State(ctx); err != nil || state != runtime.Running {
		t.Fatalf("state=%s err=%v", state, err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err := hostlinux.Read(pid)
		if err == nil && got.PGID == pid {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("process did not move to its own group: %+v err=%v", got, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := rt.Signal(ctx, runtime.Terminate); !errors.Is(err, direct.ErrIdentityChanged) {
		t.Fatalf("err=%v", err)
	}
	if _, err := hostlinux.Read(pid); err != nil {
		t.Fatalf("process was signalled: %v", err)
	}
}

// startExternal launches a helper outside any Runtime and returns its PID and a
// cleanup function.
func startExternal(t *testing.T, mode, pidfile string) (int, func()) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
	env := append(os.Environ(), "MCWARDEN_DIRECT_HELPER=1", "MCWARDEN_DIRECT_MODE="+mode)
	if pidfile != "" {
		env = append(env, "MCWARDEN_DIRECT_PIDFILE="+pidfile)
	}
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start external: %v", err)
	}
	reaped := make(chan struct{})
	go func() {
		cmd.Wait()
		close(reaped)
	}()
	return cmd.Process.Pid, func() {
		// Kill the whole group so children do not linger.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-reaped
	}
}

// startExternalUngrouped launches a helper in the caller's process group. It is
// used when a test needs the helper to change groups itself.
func startExternalUngrouped(t *testing.T, mode, pidfile string) (int, func()) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
	env := append(os.Environ(), "MCWARDEN_DIRECT_HELPER=1", "MCWARDEN_DIRECT_MODE="+mode)
	if pidfile != "" {
		env = append(env, "MCWARDEN_DIRECT_PIDFILE="+pidfile)
	}
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		t.Fatalf("start external: %v", err)
	}
	reaped := make(chan struct{})
	go func() {
		cmd.Wait()
		close(reaped)
	}()
	return cmd.Process.Pid, func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
		<-reaped
	}
}

func readStat(t *testing.T, pid int) hostlinux.Process {
	t.Helper()
	stat, err := hostlinux.Read(pid)
	if err != nil {
		t.Fatalf("read stat %d: %v", pid, err)
	}
	return stat
}

func waitForPid(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(string(bytes.TrimSpace(data))); err == nil {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid file %s was not written", path)
	return 0
}

func waitGone(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := hostlinux.Read(pid); hostlinux.IsGone(err) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid %d still present", pid)
}
