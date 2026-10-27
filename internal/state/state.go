//go:build linux

// Package state persists the process identity of a server generation so a
// later MCWarden invocation can tell whether the process it started is still
// the same one. It is deliberately separate from direct.pidfile: the pidfile is
// external process discovery, while this state records MCWarden-owned or
// adopted generation identity.
package state

import (
	"errors"
	"time"
)

// Status classifies a persisted generation against the live host.
type Status string

const (
	// Absent means no state file exists.
	Absent Status = "absent"
	// Managed means the recorded PID, start time, process group, and boot id
	// still match a live process.
	Managed Status = "managed"
	// Stale means the process is gone or the recorded identity no longer
	// matches. It must be cleaned explicitly before a new process starts.
	Stale Status = "stale"
	// Unknown means the identity could not be checked, for example because
	// procfs or the boot id was unreadable. It must never be auto-cleaned or
	// treated as stopped.
	Unknown Status = "unknown"
)

var (
	// ErrUnavailable is returned when no safe runtime directory is available.
	ErrUnavailable = errors.New("state directory unavailable")
	// ErrBusy is returned when another operation holds the server lock.
	ErrBusy = errors.New("another mcwarden operation is in progress")
	// ErrCorrupt is returned when a state file cannot be understood or does not
	// describe a complete process identity.
	ErrCorrupt = errors.New("corrupt state file")
	// ErrManaged is returned when an operation refuses to remove state that
	// still matches a live process.
	ErrManaged = errors.New("managed process is still running")
	// ErrUnknown is returned when state cannot be verified, so removing it or
	// starting another process could be unsafe.
	ErrUnknown = errors.New("process state is unknown")
)

// Identity is the process generation to persist.
type Identity struct {
	PID         int
	PGID        int
	StartTime   uint64
	Owned       bool
	SignalGroup bool
}

// Record is the JSON document stored for one server.
type Record struct {
	Version     int       `json:"version"`
	Server      string    `json:"server"`
	Runtime     string    `json:"runtime"`
	PID         int       `json:"pid"`
	PGID        int       `json:"pgid"`
	StartTime   uint64    `json:"start_time"`
	BootID      string    `json:"boot_id"`
	Owned       bool      `json:"owned"`
	SignalGroup bool      `json:"signal_group,omitempty"`
	SavedAt     time.Time `json:"saved_at"`
}

// Inspection is the result of checking persisted state against the host.
type Inspection struct {
	Status Status  `json:"status"`
	Reason string  `json:"reason,omitempty"`
	Path   string  `json:"path,omitempty"`
	Record *Record `json:"record,omitempty"`
}
