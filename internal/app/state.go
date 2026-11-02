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

// mutation is a lifecycle operation's exclusive hold on one server. The lock
// serializes start, stop, restart, kill, and state clean across invocations;
// the store is reused for every state read and write inside the operation so
// helpers never need to lock again.
type mutation struct {
	store *state.Store
	lock  *state.Lock
}

// beginMutation acquires the operation lock independently of process state.
// StateStore-only embeddings retain the historical store lock fallback.
func (s Service) beginMutation() (*mutation, error) {
	if s.OperationLock != nil {
		lock, err := s.OperationLock()
		if err != nil {
			return nil, fmt.Errorf("operation lock: %w", err)
		}
		m := &mutation{lock: lock}
		if s.StateStore != nil {
			m.store, err = s.StateStore()
			if err != nil {
				m.release()
				return nil, fmt.Errorf("state: %w", err)
			}
		}
		return m, nil
	}
	if s.StateStore == nil {
		return &mutation{}, nil
	}
	store, err := s.StateStore()
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	lock, err := store.Lock()
	if err != nil {
		return nil, fmt.Errorf("state lock: %w", err)
	}
	return &mutation{store: store, lock: lock}, nil
}

func (m *mutation) release() {
	if m != nil && m.lock != nil {
		_ = m.lock.Unlock()
	}
}

// cleanExited removes the persisted identity after this operation positively
// observed the generation exit. Only stale state may be removed: managed state
// means another live generation owns the record, and unknown state means the
// process could not be verified and must not be forgotten.
func (m *mutation) cleanExited() error {
	if m.store == nil {
		return nil
	}
	inspection, err := m.store.Inspect()
	if err != nil {
		return fmt.Errorf("state: %w", err)
	}
	switch inspection.Status {
	case state.Absent:
		return nil
	case state.Stale:
		return m.store.Clean()
	case state.Managed:
		return fmt.Errorf("%w: %s", state.ErrManaged, inspection.Reason)
	case state.Unknown:
		return fmt.Errorf("%w: %s", state.ErrUnknown, inspection.Reason)
	default:
		return fmt.Errorf("unexpected state status %q", inspection.Status)
	}
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
// no-op. The explicit command remains for state left behind by a process that
// exited outside MCWarden's control.
func (s Service) CleanState(ctx context.Context) (state.Inspection, error) {
	if err := ctx.Err(); err != nil {
		return state.Inspection{}, err
	}
	if s.StateStore == nil {
		return state.Inspection{}, fmt.Errorf("state is not configured: %w", ErrUnsupported)
	}
	m, err := s.beginMutation()
	if err != nil {
		return state.Inspection{}, err
	}
	defer m.release()

	inspection, err := m.store.Inspect()
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
		if err := m.store.Clean(); err != nil {
			return inspection, err
		}
		return state.Inspection{Status: state.Absent, Path: m.store.Path(), Reason: "stale state removed"}, nil
	default:
		return inspection, fmt.Errorf("unexpected state status %q", inspection.Status)
	}
}
