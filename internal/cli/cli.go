// Package cli adapts the core service to text/JSON output and process exit codes.
package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/liquidcatmofu/mcwarden/internal/app"
)

const usage = `Usage: mcwarden <command> [options]

Commands: status, stop, restart, version, help
Common options: --json
Stop: --stop-timeout=120s
Restart: --stop-timeout=120s --term [--term-timeout=30s]
         --kill --force [--kill-timeout=10s]

Runtime adapters are not yet included. Lifecycle commands require an adapter.
`

type response struct {
	Data any `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
	ExitCode int `json:"exit_code"`
}

func Run(ctx context.Context, args []string, out, stderr io.Writer, service app.Service) int {
	if len(args) == 0 { _, err := io.WriteString(out, usage); return app.ExitCode(err) }
	command := args[0]
	if command == "help" || command == "--help" || command == "-h" {
		_, err := io.WriteString(out, usage)
		return app.ExitCode(err)
	}
	if command == "version" || command == "--version" {
		if len(args) != 1 { fmt.Fprintln(stderr, "version accepts no arguments"); return 2 }
		_, err := fmt.Fprintln(out, "mcwarden dev")
		return app.ExitCode(err)
	}
	if command != "status" && command != "stop" && command != "restart" {
		fmt.Fprintf(stderr, "unknown command: %s\n", command)
		return 2
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	asJSON := flags.Bool("json", false, "emit JSON")
	var stopTimeout, termTimeout, killTimeout time.Duration
	var term, kill, force bool
	if command != "status" {
		flags.DurationVar(&stopTimeout, "stop-timeout", 120*time.Second, "graceful stop timeout")
	}
	if command == "restart" {
		flags.BoolVar(&term, "term", false, "send SIGTERM after graceful timeout")
		flags.BoolVar(&kill, "kill", false, "escalate through SIGTERM to SIGKILL")
		flags.BoolVar(&force, "force", false, "authorize SIGKILL (requires --kill)")
		flags.DurationVar(&termTimeout, "term-timeout", 30*time.Second, "SIGTERM wait timeout")
		flags.DurationVar(&killTimeout, "kill-timeout", 10*time.Second, "SIGKILL wait timeout")
	}
	if err := flags.Parse(args[1:]); err != nil {
		if err == flag.ErrHelp { return 0 }
		return 2
	}
	var data any
	var err error
	if flags.NArg() != 0 || (force && !kill) {
		err = app.ErrInvalid
	} else {
		switch command {
		case "status":
			data, err = service.Status(ctx)
		case "stop":
			data, err = service.Stop(ctx, app.StopOptions{Timeout: stopTimeout})
		case "restart":
			escalation := app.StopOnly
			if term { escalation = app.WithTerm }
			if kill { escalation = app.WithKill }
			data, err = service.Restart(ctx, app.RestartOptions{
				StopTimeout: stopTimeout, TermTimeout: termTimeout, KillTimeout: killTimeout,
				Escalation: escalation, AllowSIGKILL: force,
			})
		}
	}
	code := app.ExitCode(err)
	if *asJSON {
		result := response{Data: data, ExitCode: code}
		if err != nil { result.Error = err.Error() }
		if writeErr := json.NewEncoder(out).Encode(result); writeErr != nil { return app.ExitCode(writeErr) }
	} else {
		if err != nil { fmt.Fprintln(stderr, err) }
		if data != nil {
			var writeErr error
			switch value := data.(type) {
			case app.Status:
				_, writeErr = fmt.Fprintf(out, "Runtime  %s\n", value.Runtime.State)
				if writeErr == nil && value.Minecraft.Error != "" { _, writeErr = fmt.Fprintf(out, "Minecraft  %s\n", value.Minecraft.Error) }
			case app.Result:
				for _, phase := range value.Phases {
					if _, writeErr = fmt.Fprintln(out, phase); writeErr != nil { break }
				}
			}
			if writeErr != nil { return app.ExitCode(writeErr) }
		}
	}
	return code
}
