package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/liquidcatmofu/mcwarden/internal/app"
	"github.com/liquidcatmofu/mcwarden/internal/state"
)

func staticService(service app.Service) Loader {
	return func(string, string) (app.Service, error) { return service, nil }
}

func TestRun(t *testing.T) {
	for _, tt := range []struct {
		args []string
		code int
		json bool
	}{
		{[]string{"help"}, 0, false},
		{[]string{"version"}, 0, false},
		{[]string{"unknown"}, 2, false},
		{[]string{"status", "--json"}, 5, true},
		{[]string{"start", "--json"}, 5, true},
		{[]string{"start", "--no-wait", "--json"}, 5, true},
		{[]string{"start", "--start-timeout=0s", "--json"}, 2, true},
		{[]string{"stop", "--json"}, 5, true},
		{[]string{"restart", "--kill", "--json"}, 2, true},
		{[]string{"restart", "--force", "--json"}, 2, true},
		{[]string{"restart", "--kill", "--force", "--json"}, 5, true},
		{[]string{"stop", "--stop-timeout=0s", "--json"}, 2, true},
		{[]string{"stop", "--term"}, 2, false},
		{[]string{"restart", "--no-wait", "--json"}, 5, true},
		{[]string{"restart", "--start-timeout=0s", "--json"}, 2, true},
		{[]string{"stop", "--no-wait"}, 2, false},
		{[]string{"restart", "-y"}, 2, false},
		{[]string{"status", "--json", "extra"}, 2, true},
	} {
		t.Run(join(tt.args), func(t *testing.T) {
			var out, stderr bytes.Buffer
			code := Run(context.Background(), tt.args, &out, &stderr, staticService(app.Service{}))
			if code != tt.code {
				t.Fatalf("code=%d want=%d stdout=%s stderr=%s", code, tt.code, &out, &stderr)
			}
			if tt.json {
				var result response
				if err := json.Unmarshal(out.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if result.ExitCode != code || (code != 0 && result.Error == "") || stderr.Len() != 0 {
					t.Fatalf("response=%+v stderr=%s", result, &stderr)
				}
			}
		})
	}
}

func TestGlobalFlagsAcceptBothPositions(t *testing.T) {
	cases := [][]string{
		{"-c", "/tmp/mc.toml", "-s", "rail", "status", "--json"},
		{"status", "-c", "/tmp/mc.toml", "-s", "rail", "--json"},
		{"--config=/tmp/mc.toml", "--server=rail", "status", "--json"},
		{"--json", "--server", "rail", "--config", "/tmp/mc.toml", "status"},
	}
	for _, args := range cases {
		t.Run(join(args), func(t *testing.T) {
			var gotConfig, gotServer string
			load := func(configPath, serverName string) (app.Service, error) {
				gotConfig, gotServer = configPath, serverName
				return app.Service{}, nil
			}
			var out, stderr bytes.Buffer
			code := Run(context.Background(), args, &out, &stderr, load)
			if code != 5 {
				t.Fatalf("code=%d stderr=%s", code, &stderr)
			}
			if gotConfig != "/tmp/mc.toml" || gotServer != "rail" {
				t.Fatalf("config=%q server=%q", gotConfig, gotServer)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr=%s", &stderr)
			}
		})
	}
}

func TestGlobalFlagMissingValue(t *testing.T) {
	var out, stderr bytes.Buffer
	code := Run(context.Background(), []string{"status", "-c"}, &out, &stderr, staticService(app.Service{}))
	if code != 2 || stderr.Len() == 0 {
		t.Fatalf("code=%d stderr=%s", code, &stderr)
	}
}

func TestLoaderError(t *testing.T) {
	load := func(string, string) (app.Service, error) {
		return app.Service{}, fmt.Errorf("%w: boom", app.ErrConfig)
	}
	var out, stderr bytes.Buffer
	code := Run(context.Background(), []string{"status", "--json"}, &out, &stderr, load)
	if code != 2 || stderr.Len() != 0 {
		t.Fatalf("code=%d stderr=%s", code, &stderr)
	}
	var result response
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 2 || result.Error == "" {
		t.Fatalf("response=%+v", result)
	}
}

func TestHelpAndVersionSkipLoader(t *testing.T) {
	load := func(string, string) (app.Service, error) {
		t.Fatal("loader must not be called")
		return app.Service{}, nil
	}
	for _, args := range [][]string{{"help"}, {"--help"}, {"-h"}, {"version"}} {
		var out, stderr bytes.Buffer
		if code := Run(context.Background(), args, &out, &stderr, load); code != 0 {
			t.Fatalf("%v: code=%d stderr=%s", args, code, &stderr)
		}
	}
}

func TestStateCommands(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("XDG_STATE_HOME", stateDir)
	service := app.Service{StateStore: func() (*state.Store, error) { return state.Open("rail") }}

	for _, args := range [][]string{
		{"state", "show", "--json"},
		{"--json", "state", "show"},
		{"state", "clean", "--json"},
	} {
		t.Run(join(args), func(t *testing.T) {
			var out, stderr bytes.Buffer
			code := Run(context.Background(), args, &out, &stderr, staticService(service))
			if code != 0 || stderr.Len() != 0 {
				t.Fatalf("code=%d stderr=%s", code, &stderr)
			}
			var result struct {
				Data     state.Inspection `json:"data"`
				ExitCode int              `json:"exit_code"`
			}
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.ExitCode != 0 || result.Data.Status != state.Absent {
				t.Fatalf("result=%+v", result)
			}
		})
	}
}

func TestStateCommandErrors(t *testing.T) {
	for _, args := range [][]string{
		{"state"},
		{"state", "bogus"},
		{"state", "show", "extra"},
	} {
		t.Run(join(args), func(t *testing.T) {
			var out, stderr bytes.Buffer
			code := Run(context.Background(), args, &out, &stderr, staticService(app.Service{}))
			if code != 2 || stderr.Len() == 0 {
				t.Fatalf("code=%d stderr=%s", code, &stderr)
			}
		})
	}
}

func TestStateLoaderError(t *testing.T) {
	load := func(string, string) (app.Service, error) {
		return app.Service{}, fmt.Errorf("%w: boom", app.ErrConfig)
	}
	var out, stderr bytes.Buffer
	code := Run(context.Background(), []string{"state", "show", "--json"}, &out, &stderr, load)
	if code != 2 || stderr.Len() != 0 {
		t.Fatalf("code=%d stderr=%s", code, &stderr)
	}
}

func join(args []string) string { b, _ := json.Marshal(args); return string(b) }
