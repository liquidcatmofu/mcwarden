// Package minecraft describes Minecraft-specific control independently of runtime.
package minecraft

import "context"

type Capabilities struct {
	Command bool `json:"command"`
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
 Unknown State = "unknown"
 Starting State = "starting"
 Running State = "running"
 Stopping State = "stopping"
 Stopped State = "stopped"
)

// Observer is optional. State must honor cancellation and report observations
// for the current Runtime process generation only. Missing or stale evidence
// must yield Unknown; process liveness alone does not establish Running.
// The Readiness capability promises that this observer can detect startup completion.
type Observer interface {
 State(context.Context) (State, error)
}
