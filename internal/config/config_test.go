package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad(t *testing.T) {
	path := writeConfig(t, `
default_server = "rail"

[servers.rail]
runtime = "direct"

[servers.rail.direct]
command = "/srv/minecraft/run.sh"
args = ["--nogui"]
dir = "/srv/minecraft"
env = ["A=B"]
pidfile = "/run/mc.pid"
signal_group = true

[servers.test]
runtime = "direct"

[servers.test.direct]
command = "/srv/test/run.sh"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.DefaultServer != "rail" || len(cfg.Servers) != 2 {
		t.Fatalf("config=%+v", cfg)
	}
	rail := cfg.Servers["rail"]
	if rail.Runtime != "direct" || rail.Direct == nil {
		t.Fatalf("rail=%+v", rail)
	}
	d := rail.Direct
	if d.Command != "/srv/minecraft/run.sh" || d.Dir != "/srv/minecraft" || d.PidFile != "/run/mc.pid" || !d.SignalGroup {
		t.Fatalf("direct=%+v", d)
	}
	if len(d.Args) != 1 || d.Args[0] != "--nogui" || len(d.Env) != 1 || d.Env[0] != "A=B" {
		t.Fatalf("direct=%+v", d)
	}
}

func TestLoadRejectsUnknownKey(t *testing.T) {
	path := writeConfig(t, `
[servers.rail.direct]
command = "/bin/true"
commnd_typo = "x"
`)
	if _, err := Load(path); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err=%v", err)
	}
}

func TestLoadRejectsInvalid(t *testing.T) {
	for name, content := range map[string]string{
		"no servers":      ``,
		"missing default": "default_server = \"missing\"\n[servers.rail.direct]\ncommand = \"/bin/true\"\n",
		"missing direct":  "[servers.rail]\nruntime = \"direct\"\n",
		"missing command": "[servers.rail.direct]\nargs = [\"x\"]\n",
		"unknown runtime": "[servers.rail]\nruntime = \"docker\"\n",
		"invalid toml":    "[servers.rail.direct\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, content)
			if _, err := Load(path); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestLoadMinecraftRCON(t *testing.T) {
	path := writeConfig(t, `
[servers.rail.direct]
command = "/bin/true"

[servers.rail.minecraft]
control = "rcon"

[servers.rail.minecraft.rcon]
address = "127.0.0.1:25575"
password_file = "/run/mcwarden/rcon-password"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	server := cfg.Servers["rail"]
	if server.Minecraft == nil || server.Minecraft.Control != "rcon" || server.Minecraft.RCON == nil {
		t.Fatalf("minecraft=%+v", server.Minecraft)
	}
	if got := server.Minecraft.RCON.Address; got != "127.0.0.1:25575" {
		t.Fatalf("address=%q", got)
	}
	if got := server.Minecraft.RCON.PasswordFile; got != "/run/mcwarden/rcon-password" {
		t.Fatalf("password_file=%q", got)
	}
}

func TestLoadMinecraftRCONDefaultAddress(t *testing.T) {
	path := writeConfig(t, `
[servers.rail.direct]
command = "/bin/true"

[servers.rail.minecraft]
control = "rcon"

[servers.rail.minecraft.rcon]
password_file = "/run/mcwarden/rcon-password"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Servers["rail"].Minecraft.RCON.Address; got != "" {
		t.Fatalf("address should be left empty for the adapter default, got %q", got)
	}
}

func TestLoadWithoutMinecraftControl(t *testing.T) {
	path := writeConfig(t, `
[servers.rail.direct]
command = "/bin/true"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Servers["rail"].Minecraft != nil {
		t.Fatalf("minecraft=%+v", cfg.Servers["rail"].Minecraft)
	}
}

func TestLoadRejectsInvalidMinecraft(t *testing.T) {
	for name, content := range map[string]string{
		"missing rcon table": `
[servers.rail.direct]
command = "/bin/true"
[servers.rail.minecraft]
control = "rcon"
`,
		"missing password file": `
[servers.rail.direct]
command = "/bin/true"
[servers.rail.minecraft]
control = "rcon"
[servers.rail.minecraft.rcon]
address = "127.0.0.1:25575"
`,
		"unknown control": `
[servers.rail.direct]
command = "/bin/true"
[servers.rail.minecraft]
control = "console"
`,
		"missing control": `
[servers.rail.direct]
command = "/bin/true"
[servers.rail.minecraft.rcon]
password_file = "/run/secret"
`,
		"unknown rcon key": `
[servers.rail.direct]
command = "/bin/true"
[servers.rail.minecraft]
control = "rcon"
[servers.rail.minecraft.rcon]
password_file = "/run/secret"
passwrod = "typo"
`,
	} {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, content)
			if _, err := Load(path); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.toml")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v", err)
	}
}

func TestSelect(t *testing.T) {
	cfg := Config{
		DefaultServer: "rail",
		Servers: map[string]Server{
			"rail": {Direct: &Direct{Command: "/bin/true"}},
			"test": {Direct: &Direct{Command: "/bin/true"}},
		},
	}
	if name, _, err := cfg.Select(""); err != nil || name != "rail" {
		t.Fatalf("default: name=%q err=%v", name, err)
	}
	if name, _, err := cfg.Select("test"); err != nil || name != "test" {
		t.Fatalf("explicit: name=%q err=%v", name, err)
	}
	if _, _, err := cfg.Select("missing"); !errors.Is(err, ErrUnknownServer) {
		t.Fatalf("missing: err=%v", err)
	}

	noDefault := Config{Servers: map[string]Server{"only": {Direct: &Direct{Command: "/bin/true"}}}}
	if name, _, err := noDefault.Select(""); err != nil || name != "only" {
		t.Fatalf("sole: name=%q err=%v", name, err)
	}

	ambiguous := Config{Servers: map[string]Server{"a": {}, "b": {}}}
	if _, _, err := ambiguous.Select(""); !errors.Is(err, ErrNoServer) {
		t.Fatalf("ambiguous: err=%v", err)
	}
}

func TestDefaultPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	got, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "mcwarden", "config.toml"); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
