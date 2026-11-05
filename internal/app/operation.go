package app

import (
	"context"
	"fmt"
	"time"

	"github.com/liquidcatmofu/mcwarden/internal/state"
)

// operationLedger is the subset of state.OperationStore used by lifecycle
// operations. The concrete store remains the public wiring; this interface is a
// test seam for persistence failures after an external side effect.
type operationLedger interface {
	Load() (state.OperationRecord, bool, error)
	Update(context.Context, func(state.OperationRecord, bool) (state.OperationRecord, bool, error)) (state.OperationRecord, bool, error)
}

func (s Service) operationStore() (operationLedger, error) {
	if s.ledger != nil {
		return s.ledger, nil
	}
	if s.OperationStore != nil {
		return s.OperationStore, nil
	}
	if s.Operations == nil {
		return nil, nil
	}
	store, err := s.Operations()
	if err != nil {
		return nil, fmt.Errorf("operations: %w", err)
	}
	return store, nil
}

// saveDesired persists the user's requested target state. It is called after
// validation and the mutation lock but before any external side effect. A
// later operation failure never rolls this value back.
func (s Service) saveDesired(ctx context.Context, desired state.DesiredState) error {
	ledger, err := s.operationStore()
	if err != nil {
		return err
	}
	if ledger == nil {
		return nil
	}
	_, _, err = ledger.Update(ctx, func(rec state.OperationRecord, exists bool) (state.OperationRecord, bool, error) {
		rec.Desired = desired
		return rec, true, nil
	})
	if err != nil {
		return fmt.Errorf("save desired state: %w", err)
	}
	return nil
}

// prepareExitIntent appends a prepared intent before a Minecraft stop command is
// sent. It returns false when no store is configured. An unavailable generation
// must not be invented from process liveness, so callers skip this entirely.
func (s Service) prepareExitIntent(ctx context.Context, generation string, operation state.ExitOperation) (bool, error) {
	if generation == "" {
		return false, nil
	}
	ledger, err := s.operationStore()
	if err != nil {
		return false, err
	}
	if ledger == nil {
		return false, nil
	}
	now := time.Now().UTC()
	_, _, err = ledger.Update(ctx, func(rec state.OperationRecord, exists bool) (state.OperationRecord, bool, error) {
		rec.ExitIntents = append(rec.ExitIntents, state.ExitIntent{
			Generation: generation,
			Operation:  operation,
			Phase:      state.IntentPrepared,
			PreparedAt: now,
		})
		return rec, true, nil
	})
	if err != nil {
		return false, fmt.Errorf("save prepared exit intent: %w", err)
	}
	return true, nil
}

// acceptExitIntent promotes the newest matching prepared intent after the
// Minecraft command succeeded. Failure is observation-metadata loss, not
// permission to roll back a delivered stop or to skip a restart's Start.
func (s Service) acceptExitIntent(ctx context.Context, generation string, operation state.ExitOperation) error {
	if generation == "" {
		return nil
	}
	ledger, err := s.operationStore()
	if err != nil {
		return err
	}
	if ledger == nil {
		return nil
	}
	now := time.Now().UTC()
	_, _, err = ledger.Update(ctx, func(rec state.OperationRecord, exists bool) (state.OperationRecord, bool, error) {
		for i := len(rec.ExitIntents) - 1; i >= 0; i-- {
			intent := &rec.ExitIntents[i]
			if intent.Generation == generation && intent.Operation == operation && intent.Phase == state.IntentPrepared {
				intent.Phase = state.IntentAccepted
				intent.AcceptedAt = &now
				return rec, true, nil
			}
		}
		// An accepted record already proves delivery; do not duplicate it.
		for i := len(rec.ExitIntents) - 1; i >= 0; i-- {
			intent := rec.ExitIntents[i]
			if intent.Generation == generation && intent.Operation == operation && intent.Phase == state.IntentAccepted {
				return rec, false, nil
			}
		}
		rec.ExitIntents = append(rec.ExitIntents, state.ExitIntent{
			Generation: generation,
			Operation:  operation,
			Phase:      state.IntentAccepted,
			PreparedAt: now,
			AcceptedAt: &now,
		})
		return rec, true, nil
	})
	if err != nil {
		return fmt.Errorf("save accepted exit intent: %w", err)
	}
	return nil
}

// removePreparedExitIntent is best-effort cleanup after a failed Minecraft
// command. If removal itself fails, the prepared record remains, which
// classifies as unknown rather than a false intentional exit.
func (s Service) removePreparedExitIntent(ctx context.Context, generation string, operation state.ExitOperation) error {
	if generation == "" {
		return nil
	}
	ledger, err := s.operationStore()
	if err != nil {
		return err
	}
	if ledger == nil {
		return nil
	}
	_, _, err = ledger.Update(ctx, func(rec state.OperationRecord, exists bool) (state.OperationRecord, bool, error) {
		for i := len(rec.ExitIntents) - 1; i >= 0; i-- {
			intent := rec.ExitIntents[i]
			if intent.Generation == generation && intent.Operation == operation && intent.Phase == state.IntentPrepared {
				rec.ExitIntents = append(rec.ExitIntents[:i], rec.ExitIntents[i+1:]...)
				return rec, true, nil
			}
		}
		return rec, false, nil
	})
	if err != nil {
		return fmt.Errorf("remove prepared exit intent: %w", err)
	}
	return nil
}
