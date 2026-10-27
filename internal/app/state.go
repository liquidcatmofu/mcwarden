package app

import (
	"context"
	"fmt"

	"github.com/liquidcatmofu/mcwarden/internal/state"
)

func (s Service) stateStore() (*state.Store, error) {
	if s.StateStore == nil {
		return nil, fmt.Errorf("state is not configured: %w", ErrUnsupported)
	}
	store, err := s.StateStore()
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	return store, nil
}

// StateStatus reports the persisted generation and whether it still matches a
// live process. It never changes the state.
func (s Service) StateStatus(ctx context.Context) (state.Inspection, error) {
	if err := ctx.Err(); err != nil {
		return state.Inspection{}, err
	}
	store, err := s.stateStore()
	if err != nil {
		return state.Inspection{}, err
	}
	return store.Inspect()
}

// CleanState removes stale state after an explicit request. Managed state is
// refused because the process is still alive, and unknown state is refused
// because a failed check must not enable a second start. Absent state is a
// no-op.
func (s Service) CleanState(ctx context.Context) (state.Inspection, error) {
	if err := ctx.Err(); err != nil {
		return state.Inspection{}, err
	}
	store, err := s.stateStore()
	if err != nil {
		return state.Inspection{}, err
	}
	lock, err := store.Lock()
	if err != nil {
		return state.Inspection{}, fmt.Errorf("state lock: %w", err)
	}
	defer lock.Unlock()

	inspection, err := store.Inspect()
	if err != nil {
		return inspection, fmt.Errorf("state: %w", err)
	}
	switch inspection.Status {
	case state.Absent:
		return inspection, nil
	case state.Managed:
		return inspection, fmt.Errorf("%w: %s", state.ErrManaged, inspection.Reason)
	case state.Unknown:
		return inspection, fmt.Errorf("%w: %s", state.ErrUnknown, inspection.Reason)
	case state.Stale:
		if err := store.Clean(); err != nil {
			return inspection, err
		}
		return state.Inspection{Status: state.Absent, Path: store.Path(), Reason: "stale state removed"}, nil
	default:
		return inspection, fmt.Errorf("unexpected state status %q", inspection.Status)
	}
}
