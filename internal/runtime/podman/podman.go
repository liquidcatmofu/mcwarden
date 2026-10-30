// Package podman implements rootless Podman + Quadlet + user systemd lifecycle.
// systemd owns launch and stop policy; Podman IDs identify container generations.
package podman

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/liquidcatmofu/mcwarden/internal/app"
	"github.com/liquidcatmofu/mcwarden/internal/runtime"
)

type Config struct{ Unit, Container string }

// Runner executes arguments directly, never through a shell. Output streams are
// separate so machine-readable stdout is not contaminated by diagnostics.
type Runner func(context.Context, string, ...string) (stdout, stderr []byte, err error)

type Runtime struct {
	cfg Config
	run Runner
	mu  sync.Mutex
	// Discovery pins once. Only an explicit successful Start advances the ID.
	generationID string
}

var _ runtime.Runtime = (*Runtime)(nil)

func New(cfg Config) *Runtime                          { return NewWithRunner(cfg, command) }
func NewWithRunner(cfg Config, runner Runner) *Runtime { return &Runtime{cfg: cfg, run: runner} }

func command(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// CommandError retains diagnostics for programmatic inspection, but never
// interpolates tool output (which may contain host secrets) into CLI errors.
type CommandError struct {
	Tool           string
	Stdout, Stderr []byte
	Cause          error
}

func (e *CommandError) Error() string { return e.Tool + ": external command failed" }
func (e *CommandError) Unwrap() error { return app.ErrExternal }

func (r *Runtime) call(ctx context.Context, tool string, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out, stderr, err := r.run(ctx, tool, args...)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, &CommandError{Tool: tool, Stdout: out, Stderr: stderr, Cause: err}
	}
	return out, nil
}

func external(message string) error { return fmt.Errorf("%s: %w", message, app.ErrExternal) }

type inspection struct {
	ID         string `json:"Id"`
	Status     string `json:"Status"`
	CgroupPath string `json:"CgroupPath"`
}

var fullID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Request only non-secret identity, liveness and ownership fields.
const inspectFormat = `{"Id":{{json .ID}},"Status":{{json .State.Status}},"CgroupPath":{{json .State.CgroupPath}}}`

func (r *Runtime) inspect(ctx context.Context, target string) (inspection, bool, error) {
	out, err := r.call(ctx, "podman", "inspect", "--type=container", "--format", inspectFormat, target)
	if err != nil {
		if ctx.Err() != nil {
			return inspection{}, false, ctx.Err()
		}
		// Do not infer absence from localized stderr. `container exists` has a
		// documented exit 1 for absence; other failures remain external errors.
		_, existsErr := r.call(ctx, "podman", "container", "exists", target)
		var cmdErr *CommandError
		var exit interface{ ExitCode() int }
		if errors.As(existsErr, &cmdErr) && errors.As(cmdErr.Cause, &exit) && exit.ExitCode() == 1 {
			return inspection{}, false, nil
		}
		if ctx.Err() != nil {
			return inspection{}, false, ctx.Err()
		}
		return inspection{}, false, err
	}
	var info inspection
	if json.Unmarshal(out, &info) != nil || !fullID.MatchString(info.ID) || info.Status == "" {
		return inspection{}, false, external("invalid podman inspect response")
	}
	if fullID.MatchString(target) && target != info.ID {
		return inspection{}, false, external("container identity mismatch")
	}
	return info, true, nil
}

func stateOf(info inspection) (runtime.State, error) {
	switch info.Status {
	case "running":
		return runtime.Running, nil
	case "exited", "stopped":
		return runtime.Stopped, nil
	default:
		return runtime.Unknown, fmt.Errorf("container state %q is not safely operable: %w", info.Status, app.ErrStateUnknown)
	}
}

func (r *Runtime) unitStopped(ctx context.Context, allowDeactivating bool) error {
	out, err := r.call(ctx, "systemctl", "--user", "show", "--property=LoadState", "--property=ActiveState", r.cfg.Unit)
	if err != nil {
		return err
	}
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			values[k] = v
		}
	}
	if values["LoadState"] != "loaded" {
		return external("systemd unit is not loaded")
	}
	switch values["ActiveState"] {
	case "inactive", "failed":
		return nil
	case "deactivating":
		// After Wait confirmed the old ID exited, systemd may still be doing
		// ExecStopPost/auto-remove. A start job is serialized by systemd.
		if allowDeactivating {
			return nil
		}
		return fmt.Errorf("unit is still stopping: %w", app.ErrStateUnknown)
	default:
		return fmt.Errorf("unit may still own a running or pending generation: %w", app.ErrStateUnknown)
	}
}

func (r *Runtime) State(ctx context.Context) (runtime.State, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state(ctx)
}

func (r *Runtime) state(ctx context.Context) (runtime.State, error) {
	target := r.generationID
	if target == "" {
		target = r.cfg.Container
	}
	info, exists, err := r.inspect(ctx, target)
	if err != nil {
		return runtime.Unknown, err
	}
	if !exists {
		if r.generationID == "" {
			if err := r.unitStopped(ctx, false); err != nil {
				return runtime.Unknown, err
			}
		}
		return runtime.Stopped, nil
	}
	s, err := stateOf(info)
	if s == runtime.Running && r.generationID == "" {
		r.generationID = info.ID
	}
	return s, err
}

func (r *Runtime) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, err := r.state(ctx)
	if err != nil {
		return err
	}
	if s == runtime.Running {
		return app.ErrAlreadyRunning
	}
	if s != runtime.Stopped {
		return app.ErrStateUnknown
	}
	// Explicit Start may advance the generation, but must not adopt an already
	// running replacement or start while the unit is transitioning.
	info, exists, err := r.inspect(ctx, r.cfg.Container)
	if err != nil {
		return err
	}
	if exists {
		s, err = stateOf(info)
		if err != nil {
			return err
		}
		if s == runtime.Running {
			return app.ErrAlreadyRunning
		}
	}
	if err := r.unitStopped(ctx, r.generationID != ""); err != nil {
		return err
	}
	if _, err := r.unitOwnership(ctx); err != nil {
		return err
	}
	if _, err := r.call(ctx, "systemctl", "--user", "start", r.cfg.Unit); err != nil {
		return err
	}
	info, exists, err = r.inspect(ctx, r.cfg.Container)
	if err != nil {
		return err
	}
	if !exists || info.Status != "running" {
		return external("unit start did not produce a running container")
	}
	group, err := r.unitOwnership(ctx)
	if err != nil {
		return err
	}
	if err := containerOwnership(group, info); err != nil {
		return err
	}
	r.generationID = info.ID
	return nil
}

func (r *Runtime) Wait(ctx context.Context) (runtime.ExitStatus, error) {
	r.mu.Lock()
	id := r.generationID
	r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return runtime.ExitStatus{}, err
	}
	if id == "" {
		return runtime.ExitStatus{}, app.ErrNotRunning
	}
	out, err := r.call(ctx, "podman", "wait", "--condition=exited", "--ignore", id)
	if err != nil {
		return runtime.ExitStatus{}, err
	}
	text := strings.TrimSpace(string(out))
	// --ignore reports -1 for an already removed container. Treat empty output
	// conservatively as well: verify exit/absence rather than invent 0.
	if text == "" || text == "-1" {
		info, exists, err := r.inspect(ctx, id)
		if err != nil {
			return runtime.ExitStatus{}, err
		}
		if exists && info.Status != "exited" && info.Status != "stopped" {
			return runtime.ExitStatus{}, external("wait did not confirm generation exit")
		}
		return runtime.ExitStatus{Known: false}, nil
	}
	code, err := strconv.Atoi(text)
	if err != nil || code < 0 || code > 255 {
		return runtime.ExitStatus{}, external("invalid podman wait response")
	}
	return runtime.ExitStatus{Known: true, Code: code}, nil
}

func (r *Runtime) Signal(ctx context.Context, signal runtime.Signal) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	// A Quadlet stop job may force-kill after StopTimeout, even after our
	// caller has timed out. Never leave such a job behind for TERM-only.
	if signal == runtime.Terminate {
		return fmt.Errorf("Quadlet TERM-only cannot guarantee no forced kill: %w", app.ErrUnsupported)
	}
	if signal != runtime.Kill {
		return app.ErrUnsupported
	}
	if r.generationID == "" {
		return app.ErrNotRunning
	}
	info, exists, err := r.inspect(ctx, r.generationID)
	if err != nil {
		return err
	}
	if !exists || info.Status == "exited" || info.Status == "stopped" {
		return nil
	}
	// A unit stop targets the service, not an ID. Refuse to stop a known
	// replacement. The external systemd/Podman interfaces are not transactional.
	current, exists, err := r.inspect(ctx, r.cfg.Container)
	if err != nil {
		return err
	}
	if !exists || current.ID != r.generationID {
		return fmt.Errorf("unit no longer owns pinned generation: %w", app.ErrStateUnknown)
	}
	group, err := r.unitOwnership(ctx)
	if err != nil {
		return err
	}
	if err := containerOwnership(group, info); err != nil {
		return err
	}
	if err := containerOwnership(group, current); err != nil {
		return err
	}
	if _, err := r.call(ctx, "systemctl", "--user", "stop", "--no-block", r.cfg.Unit); err != nil {
		return err
	}
	_, err = r.call(ctx, "podman", "kill", "--signal", "KILL", r.generationID)
	if err != nil && ctx.Err() == nil {
		// ExecStop may have removed the generation between stop and kill.
		info, exists, inspectErr := r.inspect(ctx, r.generationID)
		if inspectErr == nil && (!exists || info.Status == "exited" || info.Status == "stopped") {
			return nil
		}
	}
	return err
}
