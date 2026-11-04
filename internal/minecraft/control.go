// Package minecraft describes Minecraft-specific control independently of runtime.
package minecraft

import "context"

type Capabilities struct {
	Command   bool `json:"command"`
	Readiness bool `json:"readiness"`
}

// Control may be implemented by an agent, console transport, or RCON.
// Successful Command means accepted for delivery, not that shutdown completed.
type Control interface {
	Command(context.Context, string) error
	Capabilities(context.Context) (Capabilities, error)
}

// State describes Minecraft's lifecycle, independently of process liveness.
// Running means startup completed, not that the server is healthy.
type State string

const (
	Unknown  State = "unknown"
	Starting State = "starting"
	Running  State = "running"
	Stopping State = "stopping"
	Stopped  State = "stopped"
)

// Observer is optional. State must honor cancellation and report observations
// for the explicitly supplied Runtime generation key only. Missing or stale
// evidence must yield Unknown; process liveness alone does not establish
// Running. The key is opaque and must not be parsed.
type Observer interface {
	State(context.Context, string) (State, error)
}

// ReadinessObserver can create evidence for a generation. Prepare captures the
// runtime's observation boundary before Start, and Bind associates that boundary
// with the generation key returned after a successful Start. Neither method owns
// the Runtime lifecycle; a failure after Start must not signal the server.
type ReadinessObserver interface {
	Observer

	Prepare(context.Context) error
	Bind(context.Context, string) error
}
