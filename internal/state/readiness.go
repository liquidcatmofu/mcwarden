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

const readinessVersion = 1

// LogBoundary identifies a file and a byte offset within it. Device and inode
// together detect rotation/recreation; Offset is the last complete position the
// observer examined. Fingerprint hashes a small window immediately before
// Offset so a same-inode truncate that has since regrown past Offset is still
// recognized as a rewrite.
type LogBoundary struct {
	Device      uint64 `json:"device"`
	Inode       uint64 `json:"inode"`
	Offset      int64  `json:"offset"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

// ReadinessRecord is generation-scoped evidence that Minecraft startup was
// observed. It is deliberately separate from the Direct process identity
// record: it does not describe a PID and must not be used as container identity
// lifecycle state. A nil Log means the log did not exist when Prepare ran.
type ReadinessRecord struct {
	Version    int          `json:"version"`
	Generation string       `json:"generation"`
	Ready      bool         `json:"ready"`
	Log        *LogBoundary `json:"log,omitempty"`
	SavedAt    time.Time    `json:"saved_at"`
}

// ReadinessStore persists one readiness record per server under
// ${XDG_STATE_HOME:-~/.local/state}/mcwarden/readiness/<server>.json.
type ReadinessStore struct {
	dir    string
	server string
}

// OpenReadiness returns a store without creating directories or reading files.
func OpenReadiness(server string) (*ReadinessStore, error) {
	base, err := stateBase()
	if err != nil {
		return nil, err
	}
	return &ReadinessStore{dir: filepath.Join(base, "mcwarden", "readiness"), server: server}, nil
}

// Path returns the readiness evidence path for this server.
func (s *ReadinessStore) Path() string {
	return filepath.Join(s.dir, fileKey(s.server)+".json")
}

// Load reads the readiness record. A missing file is absent, not an error.
// Corrupt or incomplete evidence is an error so observers fail closed.
func (s *ReadinessStore) Load() (ReadinessRecord, bool, error) {
	data, err := os.ReadFile(s.Path())
	if errors.Is(err, os.ErrNotExist) {
		return ReadinessRecord{}, false, nil
	}
	if err != nil {
		return ReadinessRecord{}, false, fmt.Errorf("read readiness %s: %w", s.Path(), err)
	}
	var rec ReadinessRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return ReadinessRecord{}, false, fmt.Errorf("%w: %s: %v", ErrCorrupt, s.Path(), err)
	}
	if rec.Version != readinessVersion {
		return ReadinessRecord{}, false, fmt.Errorf("%w: %s: unsupported version %d", ErrCorrupt, s.Path(), rec.Version)
	}
	if rec.Generation == "" {
		return ReadinessRecord{}, false, fmt.Errorf("%w: %s: missing generation", ErrCorrupt, s.Path())
	}
	if rec.Log != nil && rec.Log.Offset < 0 {
		return ReadinessRecord{}, false, fmt.Errorf("%w: %s: negative log offset", ErrCorrupt, s.Path())
	}
	return rec, true, nil
}

// Update atomically applies a read-modify-write to this server's readiness
// record under an exclusive cross-process lock. The callback runs with the
// current record after re-reading it under that lock, so its generation and
// ready decisions cannot be based on a stale Load. Returning write=false leaves
// the record untouched.
func (s *ReadinessStore) Update(ctx context.Context, fn func(ReadinessRecord, bool) (ReadinessRecord, bool, error)) (ReadinessRecord, bool, error) {
	lock, err := s.lock(ctx)
	if err != nil {
		return ReadinessRecord{}, false, err
	}
	defer lock.Close()
	if err := ctx.Err(); err != nil {
		return ReadinessRecord{}, false, err
	}

	current, exists, err := s.Load()
	if err != nil {
		return ReadinessRecord{}, false, err
	}
	next, write, err := fn(current, exists)
	if err != nil {
		return ReadinessRecord{}, false, err
	}
	if !write {
		return current, exists, nil
	}
	next.Version = readinessVersion
	next.SavedAt = time.Now().UTC()
	if err := s.save(next); err != nil {
		return ReadinessRecord{}, false, err
	}
	return next, true, nil
}

func (s *ReadinessStore) lock(ctx context.Context) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ensureDir(s.dir); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(s.dir, "."+fileKey(s.server)+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open readiness lock: %w", err)
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("lock readiness: %w", err)
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// save atomically replaces the readiness record. The caller must hold the
// readiness lock; Update is the only caller. The directory is 0700 and the file
// is 0600, matching the process identity store.
func (s *ReadinessStore) save(rec ReadinessRecord) error {
	if rec.Generation == "" {
		return fmt.Errorf("%w: refusing to save readiness without a generation", ErrCorrupt)
	}
	if rec.Log != nil && rec.Log.Offset < 0 {
		return fmt.Errorf("%w: refusing to save a negative log offset", ErrCorrupt)
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("encode readiness: %w", err)
	}
	data = append(data, '\n')

	if err := ensureDir(s.dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, "."+fileKey(s.server)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create readiness temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod readiness temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write readiness: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync readiness: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close readiness: %w", err)
	}
	if err := os.Rename(tmpName, s.Path()); err != nil {
		return fmt.Errorf("replace readiness: %w", err)
	}
	if err := syncDir(s.dir); err != nil {
		return fmt.Errorf("sync readiness directory: %w", err)
	}
	return nil
}
