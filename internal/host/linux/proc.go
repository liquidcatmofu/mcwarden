//go:build linux

// Package linux provides Linux-specific process observation through procfs.
// It isolates host-specific parsing so runtime adapters stay portable.
package linux

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Process is a point-in-time snapshot of /proc/<pid>/stat.
type Process struct {
	PID       int
	PPID      int
	PGID      int
	State     byte
	StartTime uint64 // clock ticks since boot (stat field 22)
}

// Read returns the snapshot for pid. The returned error satisfies
// errors.Is(err, os.ErrNotExist) when the process no longer exists, which lets
// callers distinguish a gone process from an unexpected failure.
func Read(pid int) (Process, error) {
	if pid <= 0 {
		return Process{}, fmt.Errorf("invalid pid %d", pid)
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return Process{}, err
	}
	return parseStat(data)
}

// parseStat parses the fields after the process name. The name (comm) is
// wrapped in parentheses and may itself contain spaces or parentheses, so the
// split happens after the final ')'.
func parseStat(data []byte) (Process, error) {
	text := string(data)
	open := strings.IndexByte(text, '(')
	closing := strings.LastIndexByte(text, ')')
	if open < 0 || closing < open {
		return Process{}, fmt.Errorf("malformed stat: missing command name")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(text[:open]))
	if err != nil {
		return Process{}, fmt.Errorf("stat pid: %w", err)
	}
	// fields[0] is state (stat field 3); ppid, pgrp, and starttime follow.
	fields := strings.Fields(text[closing+1:])
	const startTimeIndex = 19 // stat field 22
	if len(fields) <= startTimeIndex {
		return Process{}, fmt.Errorf("malformed stat: %d fields after command name", len(fields))
	}
	process := Process{PID: pid, State: fields[0][0]}
	if process.PPID, err = strconv.Atoi(fields[1]); err != nil {
		return Process{}, fmt.Errorf("stat ppid: %w", err)
	}
	if process.PGID, err = strconv.Atoi(fields[2]); err != nil {
		return Process{}, fmt.Errorf("stat pgrp: %w", err)
	}
	if process.StartTime, err = strconv.ParseUint(fields[startTimeIndex], 10, 64); err != nil {
		return Process{}, fmt.Errorf("stat starttime: %w", err)
	}
	return process, nil
}
