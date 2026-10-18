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

// ExitStatus reports how a process exited. Code is meaningful only when Known
// is true; it is -1 when the process ended from a signal. Signaled reports
// signal termination, and Signal names it when the adapter can map it. Known is
// false when no exit status can be observed, such as for a process that was not
// started as a child.
type ExitStatus struct {
	Code     int    `json:"code"`
	Known    bool   `json:"known"`
	Signaled bool   `json:"signaled,omitempty"`
	Signal   Signal `json:"signal,omitempty"`
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
