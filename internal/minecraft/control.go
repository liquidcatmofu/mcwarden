// Package minecraft describes Minecraft-specific control independently of runtime.
package minecraft

import "context"

type Capabilities struct {
	Command bool `json:"command"`
}

// Control may be implemented by an agent, console transport, or RCON.
// Successful Command means accepted for delivery, not that shutdown completed.
type Control interface {
	Command(context.Context, string) error
	Capabilities(context.Context) (Capabilities, error)
}
