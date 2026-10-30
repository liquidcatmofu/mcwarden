// Package config loads and selects MCWarden server configuration.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

var (
	// ErrNotFound is returned when the configuration file does not exist.
	ErrNotFound = errors.New("config file not found")
	// ErrInvalid is returned when the configuration is malformed or incomplete.
	ErrInvalid = errors.New("invalid config")
	// ErrUnknownServer is returned when the requested server is not defined.
	ErrUnknownServer = errors.New("unknown server")
	// ErrNoServer is returned when no server can be selected.
	ErrNoServer = errors.New("no server selected")
)

// Config is the top-level configuration file.
type Config struct {
	DefaultServer string            `toml:"default_server"`
	Servers       map[string]Server `toml:"servers"`
}

// Server describes one managed server.
type Server struct {
	// Runtime is "direct" or "podman". Empty means "direct".
	Runtime   string     `toml:"runtime"`
	Direct    *Direct    `toml:"direct"`
	Minecraft *Minecraft `toml:"minecraft"`
}

// Minecraft describes how MCWarden sends commands to Minecraft. It is separate
// from the runtime because process liveness does not imply Minecraft readiness.
type Minecraft struct {
	// Control is the command transport. Currently only "rcon" is implemented.
	Control string `toml:"control"`
	RCON    *RCON  `toml:"rcon"`
}

// RCON describes the RCON command transport. The password is never stored in
// the configuration file or passed on the command line.
type RCON struct {
	// Address is the RCON endpoint. Empty means 127.0.0.1:25575.
	Address string `toml:"address"`
	// PasswordFile holds the RCON password. A trailing LF or CRLF is removed;
	// no other trimming is performed.
	PasswordFile string `toml:"password_file"`
}

// Direct describes a locally executed server process.
type Direct struct {
	// Command is the JVM or a wrapper that ends by exec-ing the JVM.
	Command string   `toml:"command"`
	Args    []string `toml:"args"`
	Dir     string   `toml:"dir"`
	// Env adds variables to the inherited environment.
	Env []string `toml:"env"`
	// PidFile, when set, lets MCWarden adopt an already-running process. It
	// holds "PID" or "PID STARTTIME"; the start time is read from procfs when
	// omitted.
	PidFile string `toml:"pidfile"`
	// SignalGroup allows signals to reach the adopted process group.
	SignalGroup bool `toml:"signal_group"`
}

// DefaultPath returns the user configuration path following XDG.
func DefaultPath() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "mcwarden", "config.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("%w: cannot determine home directory: %v", ErrNotFound, err)
	}
	return filepath.Join(home, ".config", "mcwarden", "config.toml"), nil
}

// Load reads path, or the default path when path is empty. Unknown keys are
// rejected so a typo does not silently disable a setting.
func Load(path string) (Config, error) {
	if path == "" {
		var err error
		if path, err = DefaultPath(); err != nil {
			return Config{}, err
		}
	}
	var cfg Config
	meta, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return Config{}, fmt.Errorf("%w: %s: %v", ErrInvalid, path, err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, key := range undecoded {
			keys[i] = key.String()
		}
		sort.Strings(keys)
		return Config{}, fmt.Errorf("%w: %s: unknown keys: %s", ErrInvalid, path, strings.Join(keys, ", "))
	}
	if err := cfg.validate(); err != nil {
		return Config{}, fmt.Errorf("%w: %s: %v", ErrInvalid, path, err)
	}
	return cfg, nil
}

func (c Config) validate() error {
	if len(c.Servers) == 0 {
		return errors.New("no servers defined")
	}
	if c.DefaultServer != "" {
		if _, ok := c.Servers[c.DefaultServer]; !ok {
			return fmt.Errorf("default_server %q is not defined", c.DefaultServer)
		}
	}
	names := make([]string, 0, len(c.Servers))
	for name := range c.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		server := c.Servers[name]
		switch server.Runtime {
		case "", "direct":
			if server.Direct == nil {
				return fmt.Errorf("server %q needs a [servers.%s.direct] table", name, name)
			}
			if server.Direct.Command == "" && server.Direct.PidFile == "" {
				return fmt.Errorf("server %q needs direct.command or direct.pidfile", name)
			}
		case "podman":
			// Accepted as a known future runtime; the adapter is not implemented.
		default:
			return fmt.Errorf("server %q has unknown runtime %q", name, server.Runtime)
		}
		if server.Minecraft != nil {
			if err := server.Minecraft.validate(name); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m Minecraft) validate(server string) error {
	switch m.Control {
	case "rcon":
		if m.RCON == nil {
			return fmt.Errorf("server %q needs a [servers.%s.minecraft.rcon] table", server, server)
		}
		if m.RCON.PasswordFile == "" {
			return fmt.Errorf("server %q needs minecraft.rcon.password_file", server)
		}
	case "":
		return fmt.Errorf("server %q needs minecraft.control", server)
	default:
		return fmt.Errorf("server %q has unknown minecraft control %q", server, m.Control)
	}
	return nil
}

// Select resolves the server name. An explicit name wins, then default_server,
// then the only defined server. It returns the selected name for messages.
func (c Config) Select(name string) (string, Server, error) {
	if name != "" {
		server, ok := c.Servers[name]
		if !ok {
			return "", Server{}, fmt.Errorf("%w: %q", ErrUnknownServer, name)
		}
		return name, server, nil
	}
	if c.DefaultServer != "" {
		return c.DefaultServer, c.Servers[c.DefaultServer], nil
	}
	if len(c.Servers) == 1 {
		for only, server := range c.Servers {
			return only, server, nil
		}
	}
	return "", Server{}, fmt.Errorf("%w: use --server", ErrNoServer)
}
