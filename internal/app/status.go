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
	State minecraft.State `json:"state"`
	Capabilities *minecraft.Capabilities `json:"capabilities"`
	Error        string                  `json:"error,omitempty"`
}

type Status struct {
	Runtime   RuntimeStatus   `json:"runtime"`
	Minecraft MinecraftStatus `json:"minecraft"`
}

// Status preserves runtime information when optional Minecraft control fails.
// Failure to determine the runtime state is an error, not a healthy empty status.
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
	if state == runtime.Stopped { status.Minecraft.State = minecraft.Stopped }
	if s.Minecraft == nil {
		status.Minecraft.Error = ErrUnsupported.Error()
		return status, nil
	}
	caps, err := s.Minecraft.Capabilities(ctx)
	if ctx.Err() != nil {
		return status, ctx.Err()
	}
	if err != nil {
		status.Minecraft.Error = err.Error()
	} else {
		status.Minecraft.Capabilities = &caps
  if observer, ok := s.Minecraft.(minecraft.Observer); ok && state == runtime.Running {
   observed, err := observer.State(ctx)
   if ctx.Err() != nil { return status, ctx.Err() }
   if err != nil { status.Minecraft.Error = err.Error() } else { status.Minecraft.State = observed }
  }
	}
	return status, nil
}
