// Package runtime describes a server process without Minecraft-specific commands.
package runtime

import "context"

type State string

const (
	Running State = "running"
	Stopped State = "stopped"
	Unknown State = "unknown"
)

// Signal is independent of the host OS. Adapters translate supported values.
type Signal string

const (
	Terminate Signal = "term"
	Kill      Signal = "kill"
)

// ExitSignal identifies the signal that terminated a process. Number is the raw
// signal number and Name is its conventional name, for example 11 and SIGSEGV.
// It describes an exit cause, not an operation request, so it is independent of
// Signal.
type ExitSignal struct {
	Number int    `json:"number"`
	Name   string `json:"name"`
}

// ExitStatus reports how a process exited. Known is false when no exit status
// can be observed, such as for a process that was not started as a child. When
// the process ended from a signal, Signal is set and Code is zero. CoreDumped is
// set when the terminating signal produced a core dump.
type ExitStatus struct {
	Known      bool        `json:"known"`
	Code       int         `json:"code,omitempty"`
	Signal     *ExitSignal `json:"signal,omitempty"`
	CoreDumped bool        `json:"core_dumped,omitempty"`
}

// State and Wait must track the server JVM, including when a container wrapper
// remains alive. Start success means process launch, not Minecraft readiness.
// Runtime methods must honor context cancellation. Wait observes process exit;
// a nonzero process exit code belongs in ExitStatus, not in the returned error.
// Wait must support retry after a timeout, and must never target a replacement
// process. Callers serialize lifecycle operations for a given server.
type Runtime interface {
	Start(context.Context) error
	State(context.Context) (State, error)
	Signal(context.Context, Signal) error
	Wait(context.Context) (ExitStatus, error)
}
