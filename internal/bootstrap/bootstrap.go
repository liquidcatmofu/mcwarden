// Package bootstrap turns configuration into an app service.
package bootstrap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/liquidcatmofu/mcwarden/internal/app"
	"github.com/liquidcatmofu/mcwarden/internal/config"
	hostlinux "github.com/liquidcatmofu/mcwarden/internal/host/linux"
	"github.com/liquidcatmofu/mcwarden/internal/minecraft"
	"github.com/liquidcatmofu/mcwarden/internal/minecraft/rcon"
	"github.com/liquidcatmofu/mcwarden/internal/runtime"
	"github.com/liquidcatmofu/mcwarden/internal/runtime/direct"
	"github.com/liquidcatmofu/mcwarden/internal/state"
)

// Service loads configPath (or the default path), selects serverName, and
// builds an app service. The runtime owns process lifecycle; an optional
// Minecraft control transport is wired separately.
func Service(configPath, serverName string) (app.Service, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return app.Service{}, configError(err)
	}
	name, server, err := cfg.Select(serverName)
	if err != nil {
		return app.Service{}, configError(err)
	}
	store, err := state.Open(name)
	if err != nil {
		return app.Service{}, fmt.Errorf("server %q: %w", name, err)
	}
	inspection, err := store.Inspect()
	if err != nil {
		return app.Service{}, fmt.Errorf("server %q: state: %w", name, err)
	}
	rt, err := runtimeFor(name, server, inspection)
	if err != nil {
		return app.Service{}, err
	}
	control, err := minecraftControlFor(name, server)
	if err != nil {
		return app.Service{}, err
	}
	return app.Service{
		Runtime:     rt,
		Minecraft:   control,
		RuntimeName: "direct",
		StateStore:  func() (*state.Store, error) { return store, nil },
	}, nil
}

// minecraftControlFor builds the configured command transport. A missing
// [servers.<name>.minecraft] table leaves control unsupported; configuration
// validation rejects a control name without its required settings. Credentials
// are read lazily by the RCON adapter so an unavailable password file does not
// block status or state inspection.
func minecraftControlFor(name string, server config.Server) (minecraft.Control, error) {
	if server.Minecraft == nil {
		return nil, nil
	}
	switch server.Minecraft.Control {
	case "rcon":
		settings := server.Minecraft.RCON
		if settings == nil {
			return nil, configError(fmt.Errorf("server %q: missing minecraft.rcon config", name))
		}
		return rcon.New(settings.Address, expandHome(settings.PasswordFile)), nil
	default:
		return nil, configError(fmt.Errorf("server %q: unknown minecraft control %q", name, server.Minecraft.Control))
	}
}

// runtimeFor builds the runtime appropriate for the persisted state:
//
//   - managed: rehydrate the recorded generation so it can be observed and
//     signalled;
//   - unknown: report Unknown and refuse all operations, because an unverified
//     identity must never be treated as operable;
//   - stale: do not fall back to a pidfile; wait for "state clean";
//   - absent: discover an external process through the optional pidfile.
func runtimeFor(name string, server config.Server, inspection state.Inspection) (runtime.Runtime, error) {
	switch server.Runtime {
	case "", "direct":
	case "podman":
		return nil, fmt.Errorf("server %q: runtime podman is not implemented: %w", name, app.ErrUnsupported)
	default:
		return nil, configError(fmt.Errorf("server %q: unknown runtime %q", name, server.Runtime))
	}
	if server.Direct == nil {
		return nil, configError(fmt.Errorf("server %q: missing direct config", name))
	}
	directConfig := server.Direct
	if inspection.Status == state.Unknown {
		return unknownRuntime{reason: inspection.Reason}, nil
	}
	var adopt *direct.Adopt
	var err error
	switch inspection.Status {
	case state.Managed:
		adopt, err = adoptFromRecord(name, inspection.Record)
	case state.Absent:
		adopt, err = adoptFromPidfile(name, directConfig)
	case state.Stale:
		// The recorded generation is not manageable. Starting is refused until
		// state clean, and a pidfile must not resurrect a different process.
	default:
		return nil, fmt.Errorf("server %q: unexpected state status %q", name, inspection.Status)
	}
	if err != nil {
		return nil, err
	}
	return direct.New(direct.Config{
		Path:  expandHome(directConfig.Command),
		Args:  directConfig.Args,
		Dir:   expandHome(directConfig.Dir),
		Env:   mergeEnv(os.Environ(), directConfig.Env),
		Adopt: adopt,
	}), nil
}

// unknownRuntime stands in for persisted identity that could not be verified.
// It keeps status and state show usable while refusing every operation that
// could start or signal a process. In particular, a boot id that cannot be read
// must not be treated as permission to signal a PID that merely looks right.
type unknownRuntime struct {
	reason string
}

var _ runtime.Runtime = unknownRuntime{}

func (r unknownRuntime) Start(context.Context) error { return r.err() }
func (r unknownRuntime) State(context.Context) (runtime.State, error) {
	return runtime.Unknown, nil
}
func (r unknownRuntime) Signal(context.Context, runtime.Signal) error { return r.err() }
func (r unknownRuntime) Wait(context.Context) (runtime.ExitStatus, error) {
	return runtime.ExitStatus{}, r.err()
}

func (r unknownRuntime) err() error {
	if r.reason == "" {
		return state.ErrUnknown
	}
	return fmt.Errorf("%w: %s", state.ErrUnknown, r.reason)
}

func adoptFromRecord(name string, rec *state.Record) (*direct.Adopt, error) {
	if rec == nil {
		return nil, fmt.Errorf("server %q: managed state has no process record", name)
	}
	return &direct.Adopt{
		PID:         rec.PID,
		PGID:        rec.PGID,
		StartTime:   rec.StartTime,
		Owned:       rec.Owned,
		SignalGroup: rec.SignalGroup,
	}, nil
}

// adoptFromPidfile discovers an already-running process. A missing pidfile or a
// missing process means the server is not running, so no adoption is configured.
func adoptFromPidfile(name string, directConfig *config.Direct) (*direct.Adopt, error) {
	if directConfig.PidFile == "" {
		return nil, nil
	}
	path := expandHome(directConfig.PidFile)
	data, err := os.ReadFile(path)
	if hostlinux.IsGone(err) {
		return nil, nil
	}
	if err != nil {
		return nil, configError(fmt.Errorf("server %q: read pidfile %s: %v", name, path, err))
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return nil, configError(fmt.Errorf("server %q: pidfile %s is empty", name, path))
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 0 {
		return nil, configError(fmt.Errorf("server %q: pidfile %s has invalid pid %q", name, path, fields[0]))
	}
	var startTime uint64
	if len(fields) > 1 {
		startTime, err = strconv.ParseUint(fields[1], 10, 64)
		if err != nil || startTime == 0 {
			return nil, configError(fmt.Errorf("server %q: pidfile %s has invalid start time %q", name, path, fields[1]))
		}
	} else {
		stat, err := hostlinux.Read(pid)
		if hostlinux.IsGone(err) {
			return nil, nil
		}
		if err != nil {
			return nil, configError(fmt.Errorf("server %q: read process %d: %v", name, pid, err))
		}
		startTime = stat.StartTime
	}
	return &direct.Adopt{PID: pid, StartTime: startTime, SignalGroup: directConfig.SignalGroup}, nil
}

func configError(err error) error {
	return fmt.Errorf("%w: %v", app.ErrConfig, err)
}

func expandHome(path string) string {
	if path == "" || path[0] != '~' {
		return path
	}
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}

// mergeEnv adds extra variables over the inherited environment. Later values
// for the same key replace earlier ones. An empty extra set inherits unchanged.
func mergeEnv(base, extra []string) []string {
	if len(extra) == 0 {
		return nil
	}
	out := make([]string, 0, len(base)+len(extra))
	index := make(map[string]int, len(base)+len(extra))
	add := func(entry string) {
		key := entry
		if i := strings.IndexByte(entry, '='); i >= 0 {
			key = entry[:i]
		}
		if i, ok := index[key]; ok {
			out[i] = entry
			return
		}
		index[key] = len(out)
		out = append(out, entry)
	}
	for _, entry := range base {
		add(entry)
	}
	for _, entry := range extra {
		add(entry)
	}
	return out
}
