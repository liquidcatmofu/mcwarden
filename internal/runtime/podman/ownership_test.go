package podman

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/liquidcatmofu/mcwarden/internal/app"
	"github.com/liquidcatmofu/mcwarden/internal/runtime"
)

func TestDiscoveryOwnershipGatesGracefulMutations(t *testing.T) {
	for _, scenario := range []string{"wrong Quadlet unit", "cgroup mismatch", "ownership CLI failure", "unsupported unit"} {
		for _, operation := range []string{"state", "status", "stop", "restart"} {
			t.Run(scenario+"/"+operation, func(t *testing.T) {
				settings := cfg
				meta := ownershipStep(t)
				want := app.ErrStateUnknown
				switch scenario {
				case "wrong Quadlet unit":
					settings.Unit = "minecraft-other.service"
					meta.args[len(meta.args)-1] = settings.Unit
					meta.out = strings.ReplaceAll(meta.out, cfg.Unit, settings.Unit)
					changeFragment(t, meta, "[Service]\nExecStart=/usr/bin/podman run --name minecraft-other image\n")
					want = app.ErrConfig
				case "cgroup mismatch":
					meta.out = strings.Replace(meta.out, "ControlGroup="+serviceGroup, "ControlGroup=/user.slice/other.service", 1)
				case "ownership CLI failure":
					meta.err = exitError(1)
					want = app.ErrExternal
				case "unsupported unit":
					meta.out = strings.Replace(meta.out, "NeedDaemonReload=no", "NeedDaemonReload=yes", 1)
					want = app.ErrUnsupported
				}
				// The runner allows only read-only discovery calls. Any Wait,
				// Start, systemd stop or Podman signal would fail this test.
				r := scripted(t, inspectStep(cfg.Container, idA, "running"), meta)
				r.cfg = settings
				mc := control{command: func(command string) error {
					t.Fatalf("ownership failure must not send RCON %q", command)
					return nil
				}}
				service := app.Service{Runtime: r, Minecraft: mc}
				var err error
				switch operation {
				case "state":
					var observed runtime.State
					observed, err = r.State(ctx)
					if observed != runtime.Unknown {
						t.Fatalf("state=%s", observed)
					}
				case "status":
					status, statusErr := service.Status(ctx)
					err = statusErr
					if status.Runtime.State != runtime.Unknown {
						t.Fatalf("status=%+v", status)
					}
				case "stop":
					_, err = service.Stop(ctx, app.StopOptions{Timeout: time.Second})
				case "restart":
					_, err = service.Restart(ctx, app.RestartOptions{NoWait: true, StopTimeout: time.Second, TermTimeout: time.Second, KillTimeout: time.Second, Escalation: app.WithKill, AllowSIGKILL: true})
				}
				if !errors.Is(err, want) {
					t.Fatalf("error=%v want=%v", err, want)
				}
				if r.generationID != "" {
					t.Fatal("unverified generation was pinned")
				}
			})
		}
	}
}

func changeFragment(t *testing.T, metadata step, contents string) {
	t.Helper()
	for _, line := range strings.Split(metadata.out, "\n") {
		if path, ok := strings.CutPrefix(line, "FragmentPath="); ok {
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatal("fixture has no fragment path")
}

func TestUnitOwnership(t *testing.T) {
	for _, tt := range []struct {
		name, old, new, fragment string
		want                     error
	}{
		{name: "valid"},
		{name: "quoted name and image arguments", fragment: "[Service]\nExecStart=/usr/bin/podman run --name \"minecraft-rail\" --replace --rm image \"argument with spaces\"\n"},
		{name: "equals name", fragment: "[Service]\nExecStart=/usr/bin/podman run --name=minecraft-rail --rm image\n"},
		{name: "wrong container", fragment: "[Service]\nExecStart=/usr/bin/podman run --name other-container --rm image\n", want: app.ErrConfig},
		{name: "ordinary service", old: "UnitFileState=generated", new: "UnitFileState=enabled", want: app.ErrConfig},
		{name: "different canonical unit", old: "Id=" + cfg.Unit, new: "Id=other.service", want: app.ErrConfig},
		{name: "not a container Quadlet", old: "rail.container", new: "rail.kube", want: app.ErrConfig},
		{name: "not loaded", old: "LoadState=loaded", new: "LoadState=not-found", want: app.ErrExternal},
		{name: "stale fragment", old: "NeedDaemonReload=no", new: "NeedDaemonReload=yes", want: app.ErrUnsupported},
		{name: "drop-in override", old: "DropInPaths=", new: "DropInPaths=/etc/systemd/user/minecraft-rail.service.d/override.conf", want: app.ErrUnsupported},
		{name: "missing fragment", old: "FragmentPath=", new: "FragmentPath=/nonexistent/", want: app.ErrStateUnknown},
		{name: "incomplete metadata", old: "NeedDaemonReload=no\n", new: "", want: app.ErrExternal},
		{name: "duplicate metadata", old: "LoadState=loaded", new: "LoadState=loaded\nLoadState=loaded", want: app.ErrExternal},
		{name: "missing ExecStart", fragment: "[Service]\nType=notify\n", want: app.ErrUnsupported},
		{name: "wrong program", fragment: "[Service]\nExecStart=/bin/echo run --name minecraft-rail image\n", want: app.ErrUnsupported},
		{name: "shell wrapper", fragment: "[Service]\nExecStart=/bin/sh -c \"podman run --name minecraft-rail image\"\n", want: app.ErrUnsupported},
		{name: "unrelated name in application args", fragment: "[Service]\nExecStart=/usr/bin/podman run --name other image --name minecraft-rail\n", want: app.ErrConfig},
		{name: "duplicate container option", fragment: "[Service]\nExecStart=/usr/bin/podman run --name minecraft-rail --name other image\n", want: app.ErrConfig},
		{name: "quoted name with whitespace", fragment: "[Service]\nExecStart=/usr/bin/podman run --name \"minecraft-rail --rm\" image\n", want: app.ErrConfig},
		{name: "expanded name", fragment: "[Service]\nExecStart=/usr/bin/podman run --name $NAME image\n", want: app.ErrUnsupported},
		{name: "single quotes", fragment: "[Service]\nExecStart=/usr/bin/podman run --name 'minecraft-rail' image\n", want: app.ErrUnsupported},
		{name: "continuation", fragment: "[Service]\nExecStart=/usr/bin/podman run \\\n --name minecraft-rail image\n", want: app.ErrUnsupported},
		{name: "multiple starts", fragment: "[Service]\nExecStart=/bin/true\nExecStart=/usr/bin/podman run --name minecraft-rail image\n", want: app.ErrUnsupported},
		{name: "reset then wrong name", fragment: "[Service]\nExecStart=/usr/bin/podman run --name minecraft-rail image\nExecStart=\nExecStart=/usr/bin/podman run --name other image\n", want: app.ErrConfig},
	} {
		t.Run(tt.name, func(t *testing.T) {
			meta := ownershipStep(t)
			if tt.fragment != "" {
				changeFragment(t, meta, tt.fragment)
			}
			if tt.old != "" {
				meta.out = strings.Replace(meta.out, tt.old, tt.new, 1)
			}
			r := scripted(t, meta)
			group, err := r.unitOwnership(ctx)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err=%v want=%v", err, tt.want)
			}
			if err == nil && group != serviceGroup {
				t.Fatal("wrong group")
			}
		})
	}
}

func TestStartOwnershipMismatchDoesNotMutateUnit(t *testing.T) {
	meta := ownershipStep(t)
	changeFragment(t, meta, "[Service]\nExecStart=/usr/bin/podman run --name other-container image\n")
	steps := append(absentSteps(cfg.Container), unitStep("inactive"))
	steps = append(steps, absentSteps(cfg.Container)...)
	steps = append(steps, unitStep("inactive"), meta)
	r := scripted(t, steps...)
	if err := r.Start(ctx); !errors.Is(err, app.ErrConfig) {
		t.Fatal(err)
	}
	if r.generationID != "" {
		t.Fatal("pinned an unowned generation")
	}
}

func TestKillOwnershipMismatchDoesNotMutateUnitOrContainer(t *testing.T) {
	for _, scenario := range []string{"different Quadlet name", "different unit cgroup", "lookalike cgroup", "missing cgroup", "ownership CLI failure"} {
		t.Run(scenario, func(t *testing.T) {
			meta := ownershipStep(t)
			pinned := inspectStep(idA, idA, "running")
			current := inspectStep(cfg.Container, idA, "running")
			switch scenario {
			case "different Quadlet name":
				changeFragment(t, meta, "[Service]\nExecStart=/usr/bin/podman run --name other image\n")
			case "different unit cgroup":
				meta.out = strings.Replace(meta.out, "ControlGroup="+serviceGroup, "ControlGroup=/user.slice/other.service", 1)
			case "lookalike cgroup":
				pinned.out = strings.ReplaceAll(pinned.out, serviceGroup+"/", serviceGroup+"-other/")
			case "missing cgroup":
				pinned.out = strings.ReplaceAll(pinned.out, serviceGroup+"/libpod-payload-"+idA, "")
			case "ownership CLI failure":
				meta.err = exitError(1)
			}
			r := scripted(t, pinned, current, meta)
			r.generationID = idA
			if err := r.Signal(ctx, runtime.Kill); err == nil {
				t.Fatal("unverified ownership accepted")
			}
		})
	}
}

func TestStartRefusesToPinGenerationOutsideUnit(t *testing.T) {
	launched := inspectStep(cfg.Container, idB, "running")
	launched.out = strings.ReplaceAll(launched.out, serviceGroup, "/user.slice/other.service")
	r := scripted(t, inspectStep(idA, idA, "exited"), inspectStep(cfg.Container, idA, "exited"), unitStep("inactive"), ownershipStep(t), startStep(), launched, ownershipStep(t))
	r.generationID = idA
	if err := r.Start(ctx); !errors.Is(err, app.ErrStateUnknown) {
		t.Fatal(err)
	}
	if r.generationID != idA {
		t.Fatal("rebound to unowned replacement")
	}
}
