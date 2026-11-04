package app

import (
	"context"
	"fmt"

	"github.com/liquidcatmofu/mcwarden/internal/minecraft"
	"github.com/liquidcatmofu/mcwarden/internal/runtime"
)

type RuntimeStatus struct {
	State runtime.State `json:"state"`
}

type MinecraftStatus struct {
	State        minecraft.State         `json:"state"`
	Capabilities *minecraft.Capabilities `json:"capabilities"`
	Error        string                  `json:"error,omitempty"`
}

type Status struct {
	Runtime   RuntimeStatus   `json:"runtime"`
	Minecraft MinecraftStatus `json:"minecraft"`
}

// Status preserves runtime information when optional Minecraft control or
// readiness observation fails. Failure to determine the runtime state is an
// error, not a healthy empty status. Capabilities compose command support from
// the Control transport and readiness support from the generation-scoped
// observer; neither is inferred from the other.
func (s Service) Status(ctx context.Context) (Status, error) {
	status := Status{Runtime: RuntimeStatus{State: runtime.Unknown}, Minecraft: MinecraftStatus{State: minecraft.Unknown}}
	if err := ctx.Err(); err != nil {
		return status, err
	}
	if s.Runtime == nil {
		return status, ErrUnsupported
	}
	state, err := s.Runtime.State(ctx)
	if err != nil {
		return status, fmt.Errorf("runtime state: %w", err)
	}
	status.Runtime.State = state
	if state == runtime.Stopped {
		status.Minecraft.State = minecraft.Stopped
	}

	var caps minecraft.Capabilities
	haveCaps := false
	if s.Minecraft != nil {
		controlCaps, err := s.Minecraft.Capabilities(ctx)
		if ctx.Err() != nil {
			return status, ctx.Err()
		}
		if err != nil {
			status.Minecraft.Error = err.Error()
		} else {
			caps = controlCaps
			haveCaps = true
		}
	}
	if s.Observer != nil {
		caps.Readiness = true
		haveCaps = true
	}
	if haveCaps {
		caps.Readiness = s.Observer != nil
		status.Minecraft.Capabilities = &caps
	} else if s.Minecraft == nil {
		status.Minecraft.Error = ErrUnsupported.Error()
		return status, nil
	}
	if s.Observer != nil && state == runtime.Running {
		generation, ok, err := s.generation(ctx)
		if ctx.Err() != nil {
			return status, ctx.Err()
		}
		if err != nil {
			status.Minecraft.Error = err.Error()
		} else if ok {
			observed, err := s.Observer.State(ctx, generation)
			if ctx.Err() != nil {
				return status, ctx.Err()
			}
			if err != nil {
				status.Minecraft.Error = err.Error()
			} else {
				status.Minecraft.State = observed
			}
		}
	}
	return status, nil
}
