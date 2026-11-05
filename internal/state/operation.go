//go:build linux

package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	operationVersion = 1
	// maxExitIntents bounds the operation ledger. Old intents are pruned only
	// when a new intent is appended, so an observed exit keeps its evidence
	// even if another generation is operated on before classification.
	maxExitIntents = 64
)

// DesiredState is the final state the user last requested from MCWarden. It is
// not the currently observed runtime state and is never rolled back when an
// operation fails partway through.
type DesiredState string

const (
	DesiredRunning DesiredState = "running"
	DesiredStopped DesiredState = "stopped"
)

// ExitOperation is the MCWarden operation that requested a generation's exit.
type ExitOperation string

const (
	ExitStop    ExitOperation = "stop"
	ExitRestart ExitOperation = "restart"
)

// IntentPhase describes how much confidence MCWarden has that a requested stop
// was delivered to Minecraft.
type IntentPhase string

const (
	// IntentPrepared means MCWarden was about to request a stop, but Minecraft
	// command delivery is not yet proven.
	IntentPrepared IntentPhase = "prepared"
	// IntentAccepted means the Minecraft "stop" command satisfied the
	// Control.Command success contract.
	IntentAccepted IntentPhase = "accepted"
)

// ExitIntent is generation-scoped evidence that MCWarden requested an exit.
// Generation is an opaque runtime key and must never be parsed as a PID or
// container ID.
type ExitIntent struct {
	Generation string        `json:"generation"`
	Operation  ExitOperation `json:"operation"`
	Phase      IntentPhase   `json:"phase"`
	PreparedAt time.Time     `json:"prepared_at"`
	AcceptedAt *time.Time    `json:"accepted_at,omitempty"`
}

// OperationRecord is the persisted desired state and exit-intent ledger for one
// server. It is deliberately separate from process identity and readiness
// evidence.
type OperationRecord struct {
	Version     int          `json:"version"`
	Server      string       `json:"server"`
	Desired     DesiredState `json:"desired,omitempty"`
	UpdatedAt   time.Time    `json:"updated_at"`
	ExitIntents []ExitIntent `json:"exit_intents"`
}

// OperationStore persists the desired state and generation-scoped exit intents
// for one server under
// ${XDG_STATE_HOME:-~/.local/state}/mcwarden/operations/<server>.json.
type OperationStore struct {
	dir    string
	server string
}

// OpenOperations returns a store without creating directories or reading files.
func OpenOperations(server string) (*OperationStore, error) {
	base, err := stateBase()
	if err != nil {
		return nil, err
	}
	return &OperationStore{dir: filepath.Join(base, "mcwarden", "operations"), server: server}, nil
}

// Path returns the operation ledger path for this server.
func (s *OperationStore) Path() string {
	return filepath.Join(s.dir, fileKey(s.server)+".json")
}

// Load reads the operation record. A missing file is absent, not an error.
// Corrupt or incomplete evidence is an error so observers fail closed.
func (s *OperationStore) Load() (OperationRecord, bool, error) {
	data, err := os.ReadFile(s.Path())
	if errors.Is(err, os.ErrNotExist) {
		return OperationRecord{}, false, nil
	}
	if err != nil {
		return OperationRecord{}, false, fmt.Errorf("read operations %s: %w", s.Path(), err)
	}
	var rec OperationRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return OperationRecord{}, false, fmt.Errorf("%w: %s: %v", ErrCorrupt, s.Path(), err)
	}
	if err := validateOperationRecord(rec, s.server, s.Path()); err != nil {
		return OperationRecord{}, false, err
	}
	return rec, true, nil
}

// Update atomically applies a read-modify-write to this server's operation
// record under an exclusive cross-process lock. The callback runs with the
// current record after re-reading it under that lock. Returning write=false
// leaves the record untouched.
func (s *OperationStore) Update(ctx context.Context, fn func(OperationRecord, bool) (OperationRecord, bool, error)) (OperationRecord, bool, error) {
	lock, err := s.lock(ctx)
	if err != nil {
		return OperationRecord{}, false, err
	}
	defer lock.Close()
	if err := ctx.Err(); err != nil {
		return OperationRecord{}, false, err
	}

	current, exists, err := s.Load()
	if err != nil {
		return OperationRecord{}, false, err
	}
	next, write, err := fn(current, exists)
	if err != nil {
		return OperationRecord{}, false, err
	}
	if !write {
		return current, exists, nil
	}
	next.Version = operationVersion
	next.Server = s.server
	next.UpdatedAt = time.Now().UTC()
	next.ExitIntents = pruneExitIntents(next.ExitIntents)
	if err := s.save(next); err != nil {
		return OperationRecord{}, false, err
	}
	return next, true, nil
}

// FindExitIntent returns the intent that best explains generation's exit. An
// accepted intent outranks a prepared intent because only accepted proves that
// Minecraft command delivery succeeded. Within the same phase the newest
// matching intent wins. Generation comparison is exact and opaque.
func (r OperationRecord) FindExitIntent(generation string) (ExitIntent, bool) {
	var prepared ExitIntent
	havePrepared := false
	for i := len(r.ExitIntents) - 1; i >= 0; i-- {
		intent := r.ExitIntents[i]
		if intent.Generation != generation {
			continue
		}
		if intent.Phase == IntentAccepted {
			return intent, true
		}
		if !havePrepared {
			prepared = intent
			havePrepared = true
		}
	}
	return prepared, havePrepared
}

func validateOperationRecord(rec OperationRecord, server, path string) error {
	if rec.Version != operationVersion {
		return fmt.Errorf("%w: %s: unsupported version %d", ErrCorrupt, path, rec.Version)
	}
	if rec.Server != server {
		return fmt.Errorf("%w: %s: server %q does not match %q", ErrCorrupt, path, rec.Server, server)
	}
	switch rec.Desired {
	case "", DesiredRunning, DesiredStopped:
	default:
		return fmt.Errorf("%w: %s: unknown desired state %q", ErrCorrupt, path, rec.Desired)
	}
	for _, intent := range rec.ExitIntents {
		if intent.Generation == "" {
			return fmt.Errorf("%w: %s: exit intent is missing a generation", ErrCorrupt, path)
		}
		switch intent.Operation {
		case ExitStop, ExitRestart:
		default:
			return fmt.Errorf("%w: %s: unknown exit operation %q", ErrCorrupt, path, intent.Operation)
		}
		switch intent.Phase {
		case IntentPrepared:
			if intent.AcceptedAt != nil {
				return fmt.Errorf("%w: %s: prepared intent has accepted_at", ErrCorrupt, path)
			}
		case IntentAccepted:
			if intent.AcceptedAt == nil {
				return fmt.Errorf("%w: %s: accepted intent is missing accepted_at", ErrCorrupt, path)
			}
		default:
			return fmt.Errorf("%w: %s: unknown intent phase %q", ErrCorrupt, path, intent.Phase)
		}
	}
	return nil
}

func pruneExitIntents(intents []ExitIntent) []ExitIntent {
	if len(intents) <= maxExitIntents {
		return intents
	}
	pruned := make([]ExitIntent, maxExitIntents)
	copy(pruned, intents[len(intents)-maxExitIntents:])
	return pruned
}

func (s *OperationStore) lock(ctx context.Context) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ensureDir(s.dir); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(s.dir, "."+fileKey(s.server)+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open operations lock: %w", err)
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("lock operations: %w", err)
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// save atomically replaces the operation record. The caller must hold the
// operations lock; Update is the only caller. The directory is 0700 and the file
// is 0600, matching the other persistent stores.
func (s *OperationStore) save(rec OperationRecord) error {
	if err := validateOperationRecord(rec, s.server, s.Path()); err != nil {
		return err
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("encode operations: %w", err)
	}
	data = append(data, '\n')

	if err := ensureDir(s.dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, "."+fileKey(s.server)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create operations temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod operations temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write operations: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync operations: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close operations: %w", err)
	}
	if err := os.Rename(tmpName, s.Path()); err != nil {
		return fmt.Errorf("replace operations: %w", err)
	}
	if err := syncDir(s.dir); err != nil {
		return fmt.Errorf("sync operations directory: %w", err)
	}
	return nil
}
