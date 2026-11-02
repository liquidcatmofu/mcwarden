//go:build linux

package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	hostlinux "github.com/liquidcatmofu/mcwarden/internal/host/linux"
)

const version = 1

// procRead and bootID are indirected so tests can simulate host failures.
var (
	procRead = hostlinux.Read
	bootID   = hostlinux.BootID
)

// Store manages the state document for one server. Identity JSON lives in a
// persistent state directory, while the advisory lock lives in the runtime
// directory: the identity must outlive a login session, but a lock is only
// meaningful while the user is active.
type Store struct {
	dir    string
	server string
}

// Open returns a store for server. It does not create directories or read the
// state file, so a read-only command can inspect missing state without needing
// a writable runtime directory. Save and Lock create their directories.
func Open(server string) (*Store, error) {
	base, err := stateBase()
	if err != nil {
		return nil, err
	}
	return &Store{dir: filepath.Join(base, "mcwarden"), server: server}, nil
}

// stateBase follows the XDG base directory specification for persistent data.
// Unlike the runtime directory, it has no fallback beyond the user's home.
func stateBase() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		if !filepath.IsAbs(dir) {
			return "", fmt.Errorf("%w: XDG_STATE_HOME %q is not absolute", ErrUnavailable, dir)
		}
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("%w: cannot determine home directory: %v", ErrUnavailable, err)
	}
	return filepath.Join(home, ".local", "state"), nil
}

func runtimeBase() (string, error) {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		if !filepath.IsAbs(dir) {
			return "", fmt.Errorf("%w: XDG_RUNTIME_DIR %q is not absolute", ErrUnavailable, dir)
		}
		return validateRuntimeDir(dir)
	}
	dir := filepath.Join("/run/user", strconv.Itoa(os.Geteuid()))
	if _, err := os.Lstat(dir); err != nil {
		return "", fmt.Errorf("%w: XDG_RUNTIME_DIR is not set and %s is unavailable: %v", ErrUnavailable, dir, err)
	}
	return validateRuntimeDir(dir)
}

// validateRuntimeDir enforces the XDG runtime directory contract: a real
// directory owned by the current user with no group or other access.
func validateRuntimeDir(dir string) (string, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %v", ErrUnavailable, dir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: %s is not a directory", ErrUnavailable, dir)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return "", fmt.Errorf("%w: %s has permission %04o, want no group or other access", ErrUnavailable, dir, perm)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return "", fmt.Errorf("%w: %s is not owned by uid %d", ErrUnavailable, dir, os.Geteuid())
	}
	return dir, nil
}

func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("%w: create %s: %v", ErrUnavailable, dir, err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrUnavailable, dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrUnavailable, dir)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%w: %s has permission %04o, want no group or other access", ErrUnavailable, dir, perm)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("%w: %s is not owned by uid %d", ErrUnavailable, dir, os.Geteuid())
	}
	return nil
}

// Path returns the state file path for this server.
func (s *Store) Path() string { return filepath.Join(s.dir, s.key()+".json") }

// lockPath prepares the runtime directory and returns the server lock path.
// Only mutating operations take a lock, so read-only state inspection works
// even when no runtime directory is available.
func (s *Store) lockPath() (string, error) {
	base, err := runtimeBase()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "mcwarden")
	if err := ensureDir(dir); err != nil {
		return "", err
	}
	return filepath.Join(dir, s.key()+".lock"), nil
}

// key derives a filename from the server name. Simple names are used as-is;
// anything else is hashed so a name can never escape the state directory.
func (s *Store) key() string { return fileKey(s.server) }

func fileKey(server string) string {
	safe := server != "" && server != "." && server != ".."
	if safe {
		for _, r := range server {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			case r == '.', r == '_', r == '-':
			default:
				safe = false
			}
			if !safe {
				break
			}
		}
	}
	if safe {
		return server
	}
	sum := sha256.Sum256([]byte(server))
	return "server-" + hex.EncodeToString(sum[:8])
}

// Load reads the state document. A missing file is not an error; exists is
// false in that case. Corrupt or incomplete documents are errors and are never
// silently deleted.
func (s *Store) Load() (Record, bool, error) {
	data, err := os.ReadFile(s.Path())
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, fmt.Errorf("read state %s: %w", s.Path(), err)
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return Record{}, false, fmt.Errorf("%w: %s: %v", ErrCorrupt, s.Path(), err)
	}
	if rec.Version != version {
		return Record{}, false, fmt.Errorf("%w: %s: unsupported version %d", ErrCorrupt, s.Path(), rec.Version)
	}
	if rec.Server != s.server {
		return Record{}, false, fmt.Errorf("%w: %s: server %q does not match %q", ErrCorrupt, s.Path(), rec.Server, s.server)
	}
	if rec.PID <= 0 || rec.PGID <= 0 || rec.StartTime == 0 || rec.BootID == "" {
		return Record{}, false, fmt.Errorf("%w: %s: incomplete process identity", ErrCorrupt, s.Path())
	}
	return rec, true, nil
}

// Inspect classifies the persisted state. Missing state is Absent. Failed
// verification (unreadable procfs or boot id, or an unparsable document) is
// reported as Unknown or an error, never as Stale, so callers cannot conclude
// that a process is gone when they merely could not check.
func (s *Store) Inspect() (Inspection, error) {
	rec, ok, err := s.Load()
	if err != nil {
		return Inspection{Status: Unknown, Path: s.Path()}, err
	}
	if !ok {
		return Inspection{Status: Absent, Path: s.Path()}, nil
	}
	inspection := Inspection{Path: s.Path(), Record: &rec}

	boot, err := bootID()
	if err != nil {
		inspection.Status = Unknown
		inspection.Reason = fmt.Sprintf("cannot read boot id: %v", err)
		return inspection, nil
	}
	if boot != rec.BootID {
		inspection.Status = Stale
		inspection.Reason = "boot id changed since the state was saved"
		return inspection, nil
	}
	stat, err := procRead(rec.PID)
	if hostlinux.IsGone(err) {
		inspection.Status = Stale
		inspection.Reason = fmt.Sprintf("process %d is gone", rec.PID)
		return inspection, nil
	}
	if err != nil {
		inspection.Status = Unknown
		inspection.Reason = fmt.Sprintf("cannot read process %d: %v", rec.PID, err)
		return inspection, nil
	}
	if stat.StartTime != rec.StartTime {
		inspection.Status = Stale
		inspection.Reason = fmt.Sprintf("pid %d now has a different start time", rec.PID)
		return inspection, nil
	}
	switch stat.State {
	case 'Z', 'X':
		inspection.Status = Stale
		inspection.Reason = fmt.Sprintf("process %d has exited", rec.PID)
		return inspection, nil
	}
	if stat.PGID != rec.PGID {
		inspection.Status = Stale
		inspection.Reason = fmt.Sprintf("process %d moved to process group %d", rec.PID, stat.PGID)
		return inspection, nil
	}
	inspection.Status = Managed
	inspection.Reason = fmt.Sprintf("process %d identity matches", rec.PID)
	return inspection, nil
}

// Save atomically replaces the state document. The boot id is captured here so
// every record is tied to the boot that observed it.
func (s *Store) Save(runtimeName string, id Identity) error {
	if id.PID <= 0 || id.PGID <= 0 || id.StartTime == 0 {
		return fmt.Errorf("%w: refusing to save an incomplete process identity", ErrCorrupt)
	}
	boot, err := bootID()
	if err != nil {
		return fmt.Errorf("read boot id: %w", err)
	}
	rec := Record{
		Version:     version,
		Server:      s.server,
		Runtime:     runtimeName,
		PID:         id.PID,
		PGID:        id.PGID,
		StartTime:   id.StartTime,
		BootID:      boot,
		Owned:       id.Owned,
		SignalGroup: id.SignalGroup,
		SavedAt:     time.Now().UTC(),
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	data = append(data, '\n')

	if err := ensureDir(s.dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, "."+s.key()+".*.tmp")
	if err != nil {
		return fmt.Errorf("create state temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod state temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close state: %w", err)
	}
	if err := os.Rename(tmpName, s.Path()); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	if err := syncDir(s.dir); err != nil {
		return fmt.Errorf("sync state directory: %w", err)
	}
	return nil
}

// Clean removes the state document. The caller decides whether removal is
// allowed; Clean itself only performs the removal.
func (s *Store) Clean() error {
	if err := os.Remove(s.Path()); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("remove state %s: %w", s.Path(), err)
	}
	if err := syncDir(s.dir); err != nil {
		return fmt.Errorf("sync state directory: %w", err)
	}
	return nil
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// Lock takes a non-blocking exclusive lock for the server. It serializes all
// mutating lifecycle operations across CLI invocations. The lock file lives in
// the runtime directory, not next to the persistent identity document.
func (s *Store) Lock() (*Lock, error) {
	path, err := s.lockPath()
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open state lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, fmt.Errorf("lock state: %w", err)
	}
	return &Lock{file: f}, nil
}

// LockServer takes the lifecycle lock without opening a process identity store.
func LockServer(server string) (*Lock, error) {
	return (&Store{server: server}).Lock()
}

// Lock is an acquired server lock. Unlock must be called exactly once.
type Lock struct {
	file *os.File
}

// Unlock releases the lock. It is safe to call on a nil Lock.
func (l *Lock) Unlock() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	if closeErr := l.file.Close(); err == nil {
		err = closeErr
	}
	l.file = nil
	return err
}
