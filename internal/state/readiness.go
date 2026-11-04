//go:build linux

package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const readinessVersion = 1

// LogBoundary identifies a file and a byte offset within it. Device and inode
// together detect rotation/recreation; Offset is the last complete position the
// observer examined.
type LogBoundary struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	Offset int64  `json:"offset"`
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

// Save atomically replaces the readiness record. The directory is 0700 and the
// file is 0600, matching the process identity store.
func (s *ReadinessStore) Save(rec ReadinessRecord) error {
	if rec.Generation == "" {
		return fmt.Errorf("%w: refusing to save readiness without a generation", ErrCorrupt)
	}
	if rec.Log != nil && rec.Log.Offset < 0 {
		return fmt.Errorf("%w: refusing to save a negative log offset", ErrCorrupt)
	}
	rec.Version = readinessVersion
	rec.SavedAt = time.Now().UTC()
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
