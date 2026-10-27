// Package bootstrap turns configuration into an app service.
package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/liquidcatmofu/mcwarden/internal/app"
	"github.com/liquidcatmofu/mcwarden/internal/config"
	hostlinux "github.com/liquidcatmofu/mcwarden/internal/host/linux"
	"github.com/liquidcatmofu/mcwarden/internal/runtime"
	"github.com/liquidcatmofu/mcwarden/internal/runtime/direct"
	"github.com/liquidcatmofu/mcwarden/internal/state"
)

// Service loads configPath (or the default path), selects serverName, and
// builds an app service. Only the runtime is wired; Minecraft control is added
// with the console/agent adapter.
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
	rt, err := runtimeFor(name, server, store)
	if err != nil {
		return app.Service{}, err
	}
	return app.Service{
		Runtime:     rt,
		RuntimeName: "direct",
		StateStore:  func() (*state.Store, error) { return store, nil },
	}, nil
}

func runtimeFor(name string, server config.Server, store *state.Store) (runtime.Runtime, error) {
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
	adopt, err := adoptFor(name, directConfig, store)
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

// adoptFor rehydrates a persisted MCWarden-managed generation first, so a new
// invocation can observe and signal the process that an earlier invocation
// started. Persisted state takes precedence over a pidfile. Without usable
// state, an optional pidfile is read; a missing pidfile or a missing process
// means the server is not running, so no adoption is configured.
func adoptFor(name string, directConfig *config.Direct, store *state.Store) (*direct.Adopt, error) {
	persisted, err := adoptFromState(name, store)
	if err != nil {
		return nil, err
	}
	if persisted != nil {
		return persisted, nil
	}
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

// adoptFromState builds an adoption target from a managed record. Stale and
// absent state is left for status and state clean to handle. Unknown state is
// still adopted so the runtime reports the unreadable process instead of
// pretending it stopped, matching the "never treat unknown as stopped" rule.
func adoptFromState(name string, store *state.Store) (*direct.Adopt, error) {
	if store == nil {
		return nil, nil
	}
	inspection, err := store.Inspect()
	if err != nil {
		return nil, fmt.Errorf("server %q: state: %w", name, err)
	}
	switch inspection.Status {
	case state.Managed, state.Unknown:
	default:
		return nil, nil
	}
	rec := inspection.Record
	if rec == nil {
		return nil, fmt.Errorf("server %q: %s state has no process record", name, inspection.Status)
	}
	return &direct.Adopt{
		PID:         rec.PID,
		PGID:        rec.PGID,
		StartTime:   rec.StartTime,
		Owned:       rec.Owned,
		SignalGroup: rec.SignalGroup,
	}, nil
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
