package podman

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/liquidcatmofu/mcwarden/internal/app"
	"github.com/liquidcatmofu/mcwarden/internal/minecraft"
	"github.com/liquidcatmofu/mcwarden/internal/runtime"
)

var idA = strings.Repeat("a", 64)
var idB = strings.Repeat("b", 64)
var ctx = context.Background()
var cfg = Config{Unit: "minecraft-rail.service", Container: "minecraft-rail"}
var serviceGroup = "/user.slice/user-1000.slice/user@1000.service/app.slice/" + cfg.Unit

type exitError int

func (e exitError) Error() string { return "tool exit" }
func (e exitError) ExitCode() int { return int(e) }

type step struct {
	tool  string
	args  []string
	out   string
	err   error
	block bool
}

func inspectStep(target, id, status string) step {
	return step{tool: "podman", args: []string{"inspect", "--type=container", "--format", inspectFormat, target}, out: fmt.Sprintf(`{"Id":%q,"Status":%q,"CgroupPath":%q}`, id, status, serviceGroup+"/libpod-payload-"+id)}
}

func ownershipStep(t *testing.T) step {
	t.Helper()
	fragment := filepath.Join(t.TempDir(), cfg.Unit)
	if err := os.WriteFile(fragment, []byte("[Service]\nExecStart=/usr/bin/podman run --name minecraft-rail --cidfile=%t/%N.cid --replace --rm --cgroups=split --stop-timeout 180 docker.io/library/eclipse-temurin:21-jdk-noble ./run.sh nogui\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return step{tool: "systemctl", args: append(append([]string(nil), ownershipProperties...), cfg.Unit), out: "Id=" + cfg.Unit + "\nLoadState=loaded\nUnitFileState=generated\nSourcePath=/home/user/.config/containers/systemd/rail.container\nFragmentPath=" + fragment + "\nDropInPaths=\nNeedDaemonReload=no\nControlGroup=" + serviceGroup + "\n"}
}
func absentSteps(target string) []step {
	return []step{
		{tool: "podman", args: []string{"inspect", "--type=container", "--format", inspectFormat, target}, err: exitError(125)},
		{tool: "podman", args: []string{"container", "exists", target}, err: exitError(1)},
	}
}
func unitStep(active string) step {
	return step{tool: "systemctl", args: []string{"--user", "show", "--property=LoadState", "--property=ActiveState", cfg.Unit}, out: "LoadState=loaded\nActiveState=" + active + "\n"}
}
func startStep() step { return step{tool: "systemctl", args: []string{"--user", "start", cfg.Unit}} }
func stopStep() step {
	return step{tool: "systemctl", args: []string{"--user", "stop", "--no-block", cfg.Unit}}
}
func waitStep(out string) step {
	return step{tool: "podman", args: []string{"wait", "--condition=exited", "--ignore", idA}, out: out}
}
func scripted(t *testing.T, steps ...step) *Runtime {
	t.Helper()
	i := 0
	t.Cleanup(func() {
		if i != len(steps) {
			t.Errorf("consumed %d/%d commands", i, len(steps))
		}
	})
	return NewWithRunner(cfg, func(ctx context.Context, tool string, args ...string) ([]byte, []byte, error) {
		if i >= len(steps) {
			t.Fatalf("unexpected command: %s %v", tool, args)
		}
		s := steps[i]
		i++
		if tool != s.tool || !reflect.DeepEqual(args, s.args) {
			t.Fatalf("command %d: %s %v, want %s %v", i, tool, args, s.tool, s.args)
		}
		if s.block {
			<-ctx.Done()
			return nil, nil, ctx.Err()
		}
		return []byte(s.out), []byte("sensitive diagnostic"), s.err
	})
}

func TestState(t *testing.T) {
	tests := []struct {
		name    string
		steps   []step
		want    runtime.State
		wantErr bool
	}{
		{"running", []step{inspectStep(cfg.Container, idA, "running"), ownershipStep(t)}, runtime.Running, false},
		{"exited", []step{inspectStep(cfg.Container, idA, "exited")}, runtime.Stopped, false},
		{"absent", append(absentSteps(cfg.Container), unitStep("inactive")), runtime.Stopped, false},
		{"absent active", append(absentSteps(cfg.Container), unitStep("active")), runtime.Unknown, true},
		{"absent activating", append(absentSteps(cfg.Container), unitStep("activating")), runtime.Unknown, true},
		{"absent deactivating", append(absentSteps(cfg.Container), unitStep("deactivating")), runtime.Unknown, true},
		{"paused", []step{inspectStep(cfg.Container, idA, "paused")}, runtime.Unknown, true},
		{"stopping", []step{inspectStep(cfg.Container, idA, "stopping")}, runtime.Unknown, true},
		{"created", []step{inspectStep(cfg.Container, idA, "created")}, runtime.Unknown, true},
		{"inspect failure", []step{{tool: "podman", args: []string{"inspect", "--type=container", "--format", inspectFormat, cfg.Container}, err: exitError(125)}, {tool: "podman", args: []string{"container", "exists", cfg.Container}, err: exitError(125)}}, runtime.Unknown, true},
		{"invalid response", []step{{tool: "podman", args: []string{"inspect", "--type=container", "--format", inspectFormat, cfg.Container}, out: "{}"}}, runtime.Unknown, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := scripted(t, tt.steps...)
			s, err := r.State(ctx)
			if s != tt.want || (err != nil) != tt.wantErr {
				t.Fatalf("state=%s err=%v", s, err)
			}
			if s == runtime.Running && r.generationID != idA {
				t.Fatal("generation not pinned")
			}
		})
	}
}

func TestInspectFormat(t *testing.T) {
	// Podman's template field is .ID, although the JSON output key is Id.
	tmpl, err := template.New("inspect").Funcs(template.FuncMap{"json": func(v any) (string, error) { b, err := json.Marshal(v); return string(b), err }}).Parse(inspectFormat)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	data := struct {
		ID    string
		State struct{ Status, CgroupPath string }
	}{ID: idA}
	data.State.Status = "running"
	if err := tmpl.Execute(&out, data); err != nil {
		t.Fatal(err)
	}
	var info inspection
	if err := json.Unmarshal(out.Bytes(), &info); err != nil || info.ID != idA || info.Status != "running" {
		t.Fatalf("%+v %v", info, err)
	}
}

func TestMissingExecutableIsExternal(t *testing.T) {
	r := New(cfg)
	_, err := r.call(ctx, "/mcwarden-test/nonexistent-executable")
	if app.ExitCode(err) != 10 {
		t.Fatalf("err=%v", err)
	}
}

func TestMissingUnitIsExternal(t *testing.T) {
	unit := unitStep("inactive")
	unit.out = "LoadState=not-found\nActiveState=inactive\n"
	r := scripted(t, append(absentSteps(cfg.Container), unit)...)
	s, err := r.State(ctx)
	if s != runtime.Unknown || app.ExitCode(err) != 10 {
		t.Fatalf("%s %v", s, err)
	}
}

func TestWaitRequiresPin(t *testing.T) {
	r := scripted(t)
	if _, err := r.Wait(ctx); !errors.Is(err, app.ErrNotRunning) {
		t.Fatal(err)
	}
}

func TestWaitFailureIsNotContainerExit(t *testing.T) {
	bad := waitStep("")
	bad.err = exitError(125)
	r := scripted(t, bad)
	r.generationID = idA
	if _, err := r.Wait(ctx); app.ExitCode(err) != 10 {
		t.Fatal(err)
	}
}

func TestKillRequiresSuccessfulStopJob(t *testing.T) {
	bad := stopStep()
	bad.err = exitError(1)
	r := scripted(t, inspectStep(idA, idA, "running"), inspectStep(cfg.Container, idA, "running"), ownershipStep(t), bad)
	r.generationID = idA
	if err := r.Signal(ctx, runtime.Kill); app.ExitCode(err) != 10 {
		t.Fatal(err)
	}
}

func TestKillToleratesExecStopRemoval(t *testing.T) {
	bad := step{tool: "podman", args: []string{"kill", "--signal", "KILL", idA}, err: exitError(125)}
	steps := []step{inspectStep(idA, idA, "running"), inspectStep(cfg.Container, idA, "running"), ownershipStep(t), stopStep(), bad}
	steps = append(steps, absentSteps(idA)...)
	r := scripted(t, steps...)
	r.generationID = idA
	if err := r.Signal(ctx, runtime.Kill); err != nil {
		t.Fatal(err)
	}
}

func TestGenerationNeverRebinds(t *testing.T) {
	steps := []step{inspectStep(cfg.Container, idA, "running"), ownershipStep(t), inspectStep(idA, idA, "running"), inspectStep(idA, idA, "exited")}
	steps = append(steps, absentSteps(idA)...)
	r := scripted(t, steps...)
	for _, want := range []runtime.State{runtime.Running, runtime.Running, runtime.Stopped, runtime.Stopped} {
		s, err := r.State(ctx)
		if s != want || err != nil {
			t.Fatalf("%s %v", s, err)
		}
	}
	if r.generationID != idA {
		t.Fatal("rebound")
	}
	// A new invocation is allowed to discover B from the configured name.
	next := scripted(t, inspectStep(cfg.Container, idB, "running"), ownershipStep(t))
	if s, err := next.State(ctx); s != runtime.Running || err != nil || next.generationID != idB {
		t.Fatalf("new instance: %s %v", s, err)
	}
}

func TestStart(t *testing.T) {
	steps := append(absentSteps(cfg.Container), unitStep("inactive"))
	steps = append(steps, absentSteps(cfg.Container)...)
	steps = append(steps, unitStep("inactive"), ownershipStep(t), startStep(), inspectStep(cfg.Container, idB, "running"), ownershipStep(t))
	r := scripted(t, steps...)
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if r.generationID != idB {
		t.Fatal("new ID not pinned")
	}
}

func TestStartAlreadyRunning(t *testing.T) {
	r := scripted(t, inspectStep(cfg.Container, idA, "running"), ownershipStep(t))
	if err := r.Start(ctx); !errors.Is(err, app.ErrAlreadyRunning) {
		t.Fatal(err)
	}
}

func TestStartRejectsReplacement(t *testing.T) {
	r := scripted(t, inspectStep(idA, idA, "exited"), inspectStep(cfg.Container, idB, "running"))
	r.generationID = idA
	if err := r.Start(ctx); !errors.Is(err, app.ErrAlreadyRunning) {
		t.Fatal(err)
	}
	if r.generationID != idA {
		t.Fatal("adopted replacement")
	}
}

func TestSystemctlFailureIsExternal(t *testing.T) {
	bad := startStep()
	bad.err = errors.New("executable missing")
	r := scripted(t, inspectStep(cfg.Container, idA, "exited"), inspectStep(cfg.Container, idA, "exited"), unitStep("inactive"), ownershipStep(t), bad)
	err := r.Start(ctx)
	if app.ExitCode(err) != 10 || strings.Contains(err.Error(), "sensitive") {
		t.Fatal(err)
	}
	var ce *CommandError
	if !errors.As(err, &ce) || string(ce.Stderr) != "sensitive diagnostic" {
		t.Fatal("lost diagnostics")
	}
}

func TestWait(t *testing.T) {
	for _, code := range []int{0, 1, 137, 255} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			r := scripted(t, waitStep(fmt.Sprint(code)+"\n"))
			r.generationID = idA
			exit, err := r.Wait(ctx)
			if err != nil || !exit.Known || exit.Code != code || exit.Signal != nil {
				t.Fatalf("%+v %v", exit, err)
			}
		})
	}
}

func TestWaitRemoved(t *testing.T) {
	for _, output := range []string{"", "-1\n"} {
		t.Run(fmt.Sprintf("output=%q", output), func(t *testing.T) {
			steps := append([]step{waitStep(output)}, absentSteps(idA)...)
			r := scripted(t, steps...)
			r.generationID = idA
			exit, err := r.Wait(ctx)
			if err != nil || exit.Known {
				t.Fatalf("%+v %v", exit, err)
			}
		})
	}
}

func TestWaitCancellationAndRetry(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprint(timeout), func(t *testing.T) {
			blocked := waitStep("")
			blocked.block = true
			r := scripted(t, blocked, waitStep("0"))
			r.generationID = idA
			var child context.Context
			var cancel context.CancelFunc
			want := context.Canceled
			if timeout {
				child, cancel = context.WithTimeout(ctx, time.Millisecond)
				want = context.DeadlineExceeded
			} else {
				child, cancel = context.WithCancel(ctx)
				time.AfterFunc(time.Millisecond, cancel)
			}
			defer cancel()
			if _, err := r.Wait(child); !errors.Is(err, want) {
				t.Fatal(err)
			}
			if _, err := r.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			if r.generationID != idA {
				t.Fatal("lost pin")
			}
		})
	}
}

func TestKillUsesPinnedIDAfterStopJob(t *testing.T) {
	r := scripted(t, inspectStep(idA, idA, "running"), inspectStep(cfg.Container, idA, "running"), ownershipStep(t), stopStep(), step{tool: "podman", args: []string{"kill", "--signal", "KILL", idA}})
	r.generationID = idA
	if err := r.Signal(ctx, runtime.Kill); err != nil {
		t.Fatal(err)
	}
}

func TestKillDoesNotTouchReplacement(t *testing.T) {
	t.Run("removed", func(t *testing.T) {
		r := scripted(t, absentSteps(idA)...)
		r.generationID = idA
		if err := r.Signal(ctx, runtime.Kill); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("mismatch", func(t *testing.T) {
		r := scripted(t, inspectStep(idA, idA, "running"), inspectStep(cfg.Container, idB, "running"))
		r.generationID = idA
		if err := r.Signal(ctx, runtime.Kill); !errors.Is(err, app.ErrStateUnknown) {
			t.Fatal(err)
		}
	})
}

func TestTerminateUnsupportedWithoutSideEffects(t *testing.T) {
	for _, id := range []string{"", idA} {
		r := scripted(t)
		r.generationID = id
		if err := r.Signal(ctx, runtime.Terminate); app.ExitCode(err) != 5 {
			t.Fatal(err)
		}
	}
}

// This command transport stand-in tests the app/adapter boundary without an
// RCON server; protocol behavior is covered by the existing RCON adapter tests.
type control struct{ command func(string) error }

func (c control) Capabilities(context.Context) (minecraft.Capabilities, error) {
	return minecraft.Capabilities{Command: true}, nil
}
func (c control) Command(_ context.Context, command string) error { return c.command(command) }

func TestRestartLifecycle(t *testing.T) {
	for _, scenario := range []string{"graceful", "rcon failure", "term", "kill", "no escalation", "kill requires force"} {
		t.Run(scenario, func(t *testing.T) {
			steps := []step{inspectStep(cfg.Container, idA, "running"), ownershipStep(t)}
			opts := app.RestartOptions{NoWait: true, StopTimeout: time.Millisecond, TermTimeout: time.Millisecond, KillTimeout: time.Millisecond}
			wantErr := error(nil)
			switch scenario {
			case "rcon failure":
				opts.Escalation = app.WithKill
				opts.AllowSIGKILL = true
				wantErr = app.ErrExternal
			case "graceful":
				steps = append(steps, waitStep("0"))
			case "kill requires force":
				steps = nil
				opts.Escalation = app.WithKill
				wantErr = app.ErrInvalid
			default:
				blocked := waitStep("")
				blocked.block = true
				steps = append(steps, blocked)
				if scenario == "no escalation" {
					wantErr = app.ErrTimeout
					break
				}
				opts.Escalation = app.WithTerm
				if scenario == "term" {
					wantErr = app.ErrUnsupported
					break
				}
				if scenario == "kill" {
					opts.Escalation = app.WithKill
					opts.AllowSIGKILL = true
					steps = append(steps, inspectStep(idA, idA, "running"), inspectStep(cfg.Container, idA, "running"), ownershipStep(t), stopStep(), step{tool: "podman", args: []string{"kill", "--signal", "KILL", idA}})
				}
				steps = append(steps, waitStep("0"))
			}
			if wantErr == nil {
				steps = append(steps, inspectStep(idA, idA, "exited"), inspectStep(cfg.Container, idA, "exited"), unitStep("deactivating"), ownershipStep(t), startStep(), inspectStep(cfg.Container, idB, "running"), ownershipStep(t))
			}
			r := scripted(t, steps...)
			commands := 0
			mc := control{command: func(command string) error {
				commands++
				if command != "stop" {
					t.Fatal(command)
				}
				if scenario == "rcon failure" {
					return app.ErrExternal
				}
				return nil
			}}
			result, err := (app.Service{Runtime: r, Minecraft: mc}).Restart(ctx, opts)
			if !errors.Is(err, wantErr) {
				t.Fatalf("error=%v want=%v", err, wantErr)
			}
			if wantErr == nil && r.generationID != idB {
				t.Fatal("new generation not pinned")
			}
			if scenario != "kill requires force" && commands != 1 {
				t.Fatal("missing graceful stop")
			}
			for _, event := range result.Events {
				if event == app.TermSent || event == app.TermTimedOut {
					t.Fatalf("Podman must not send TERM or wait on a TERM stage: %v", result.Events)
				}
			}
		})
	}
}
