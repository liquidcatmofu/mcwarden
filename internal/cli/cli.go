// Package cli adapts the core service to text/JSON output and process exit codes.
package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/liquidcatmofu/mcwarden/internal/app"
	"github.com/liquidcatmofu/mcwarden/internal/state"
)

const usage = `Usage: mcwarden [global options] <command> [options]

Commands: status, start, stop, restart, state, version, help
Global options: -c, --config <path>  -s, --server <name>  --json
State: state show | state clean
Start: --start-timeout=120s [--no-wait]
Stop: --stop-timeout=120s
Restart: --stop-timeout=120s --term [--term-timeout=30s]
         --kill --force [--kill-timeout=10s]
         --start-timeout=120s [--no-wait]

Global options may appear before or after the command.
Runtime adapters and Minecraft RCON control are configured through TOML.
start/restart without --no-wait require a readiness observer (not yet implemented).
`

// Loader builds the service for a selected server. An empty configPath means
// the default location, and an empty serverName means the configured default.
type Loader func(configPath, serverName string) (app.Service, error)

type response struct {
	Data     any    `json:"data,omitempty"`
	Error    string `json:"error,omitempty"`
	ExitCode int    `json:"exit_code"`
}

type globalOptions struct {
	config string
	server string
	json   bool
}

func Run(ctx context.Context, args []string, out, stderr io.Writer, load Loader) int {
	globals, rest, err := parseGlobals(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return app.ExitCode(err)
	}
	if len(rest) == 0 {
		_, err := io.WriteString(out, usage)
		return app.ExitCode(err)
	}
	command := rest[0]
	if command == "help" || command == "--help" || command == "-h" {
		_, err := io.WriteString(out, usage)
		return app.ExitCode(err)
	}
	if command == "version" || command == "--version" {
		if len(rest) != 1 {
			fmt.Fprintln(stderr, "version accepts no arguments")
			return 2
		}
		_, err := fmt.Fprintln(out, "mcwarden dev")
		return app.ExitCode(err)
	}
	if command == "state" {
		return runState(ctx, rest[1:], globals, out, stderr, load)
	}
	if command != "status" && command != "start" && command != "stop" && command != "restart" {
		fmt.Fprintf(stderr, "unknown command: %s\n", command)
		return 2
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	asJSON := flags.Bool("json", false, "emit JSON")
	var stopTimeout, termTimeout, killTimeout, startTimeout time.Duration
	var term, kill, force, noWait bool
	switch command {
	case "start":
		flags.BoolVar(&noWait, "no-wait", false, "return after process launch without waiting for Minecraft startup")
		flags.DurationVar(&startTimeout, "start-timeout", 120*time.Second, "Minecraft startup wait timeout")
	case "stop":
		flags.DurationVar(&stopTimeout, "stop-timeout", 120*time.Second, "graceful stop timeout")
	case "restart":
		flags.DurationVar(&stopTimeout, "stop-timeout", 120*time.Second, "graceful stop timeout")
		flags.BoolVar(&noWait, "no-wait", false, "return after process launch without waiting for Minecraft startup")
		flags.DurationVar(&startTimeout, "start-timeout", 120*time.Second, "Minecraft startup wait timeout")
		flags.BoolVar(&term, "term", false, "send SIGTERM after graceful timeout")
		flags.BoolVar(&kill, "kill", false, "escalate through SIGTERM to SIGKILL")
		flags.BoolVar(&force, "force", false, "authorize SIGKILL (requires --kill)")
		flags.DurationVar(&termTimeout, "term-timeout", 30*time.Second, "SIGTERM wait timeout")
		flags.DurationVar(&killTimeout, "kill-timeout", 10*time.Second, "SIGKILL wait timeout")
	}
	if err := flags.Parse(rest[1:]); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	var data any
	if flags.NArg() != 0 || (force && !kill) {
		err = app.ErrInvalid
	} else {
		service, loadErr := load(globals.config, globals.server)
		if loadErr != nil {
			err = loadErr
		} else {
			switch command {
			case "status":
				data, err = service.Status(ctx)
			case "start":
				data, err = service.Start(ctx, app.StartOptions{StartTimeout: startTimeout, NoWait: noWait})
			case "stop":
				data, err = service.Stop(ctx, app.StopOptions{Timeout: stopTimeout})
			case "restart":
				escalation := app.StopOnly
				if term {
					escalation = app.WithTerm
				}
				if kill {
					escalation = app.WithKill
				}
				data, err = service.Restart(ctx, app.RestartOptions{
					StartTimeout: startTimeout, NoWait: noWait,
					StopTimeout: stopTimeout, TermTimeout: termTimeout, KillTimeout: killTimeout,
					Escalation: escalation, AllowSIGKILL: force,
				})
			}
		}
	}
	return output(out, stderr, *asJSON || globals.json, data, err)
}

// runState handles the state subcommands. They have no command-specific flags;
// global options are already extracted.
func runState(ctx context.Context, args []string, globals globalOptions, out, stderr io.Writer, load Loader) int {
	if len(args) != 1 || (args[0] != "show" && args[0] != "clean") {
		fmt.Fprintln(stderr, "usage: mcwarden state show|clean")
		return 2
	}
	var data any
	service, err := load(globals.config, globals.server)
	if err == nil {
		var inspection state.Inspection
		switch args[0] {
		case "show":
			inspection, err = service.StateStatus(ctx)
		case "clean":
			inspection, err = service.CleanState(ctx)
		}
		if err == nil || inspection.Status != "" {
			data = inspection
		}
	}
	return output(out, stderr, globals.json, data, err)
}

// output writes either the JSON envelope or the human-readable form and returns
// the mapped exit code.
func output(out, stderr io.Writer, asJSON bool, data any, err error) int {
	code := app.ExitCode(err)
	if asJSON {
		result := response{Data: data, ExitCode: code}
		if err != nil {
			result.Error = err.Error()
		}
		if writeErr := json.NewEncoder(out).Encode(result); writeErr != nil {
			return app.ExitCode(writeErr)
		}
		return code
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
	}
	if data != nil {
		var writeErr error
		switch value := data.(type) {
		case app.Status:
			_, writeErr = fmt.Fprintf(out, "Runtime  %s\nMinecraft  %s\n", value.Runtime.State, value.Minecraft.State)
			if writeErr == nil && value.Minecraft.Error != "" {
				_, writeErr = fmt.Fprintf(out, "Minecraft  %s\n", value.Minecraft.Error)
			}
		case app.Result:
			for _, phase := range value.Events {
				if _, writeErr = fmt.Fprintln(out, phase); writeErr != nil {
					break
				}
			}
		case state.Inspection:
			_, writeErr = fmt.Fprintf(out, "State  %s\n", value.Status)
			if writeErr == nil && value.Reason != "" {
				_, writeErr = fmt.Fprintf(out, "Reason  %s\n", value.Reason)
			}
			if writeErr == nil && value.Record != nil {
				record := value.Record
				_, writeErr = fmt.Fprintf(out, "Server  %s\nRuntime  %s\nPID  %d\nPGID  %d\nStart time  %d\nBoot ID  %s\n",
					record.Server, record.Runtime, record.PID, record.PGID, record.StartTime, record.BootID)
			}
		}
		if writeErr != nil {
			return app.ExitCode(writeErr)
		}
	}
	return code
}

// parseGlobals extracts global options wherever they appear, so both
// "mcwarden -s rail status" and "mcwarden status -s rail" work. A "--"
// terminates global parsing.
func parseGlobals(args []string) (globalOptions, []string, error) {
	var opts globalOptions
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "-c", "--config":
			if !hasValue {
				if i+1 >= len(args) {
					return opts, nil, fmt.Errorf("--config requires a path: %w", app.ErrInvalid)
				}
				i++
				value = args[i]
			}
			opts.config = value
		case "-s", "--server":
			if !hasValue {
				if i+1 >= len(args) {
					return opts, nil, fmt.Errorf("--server requires a name: %w", app.ErrInvalid)
				}
				i++
				value = args[i]
			}
			opts.server = value
		case "-json", "--json":
			if hasValue {
				enabled, err := strconv.ParseBool(value)
				if err != nil {
					return opts, nil, fmt.Errorf("invalid --json value %q: %w", value, app.ErrInvalid)
				}
				opts.json = enabled
			} else {
				opts.json = true
			}
		default:
			rest = append(rest, arg)
		}
	}
	return opts, rest, nil
}
