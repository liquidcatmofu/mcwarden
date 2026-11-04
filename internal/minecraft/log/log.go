//go:build linux

// Package log observes Minecraft startup completion from latest.log. It is a
// readiness observer only: it never owns or signals the server process.
package log

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"

	"github.com/liquidcatmofu/mcwarden/internal/minecraft"
	"github.com/liquidcatmofu/mcwarden/internal/state"
)

// ErrNotPrepared is returned when Bind is called without a preceding Prepare.
var ErrNotPrepared = errors.New("readiness observer was not prepared")

// Observer stores generation-scoped readiness evidence derived from a log file.
type Observer struct {
	path  string
	store *state.ReadinessStore

	mu       sync.Mutex
	prepared bool
	boundary *state.LogBoundary
}

var _ minecraft.ReadinessObserver = (*Observer)(nil)

// New returns an observer for path that persists evidence through store.
func New(path string, store *state.ReadinessStore) *Observer {
	return &Observer{path: path, store: store}
}

// Prepare records the current log boundary before a new generation starts. A
// missing log is a normal absent boundary; other access failures are errors.
func (o *Observer) Prepare(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	boundary, err := fileBoundary(o.path)
	if err != nil {
		return err
	}
	o.mu.Lock()
	o.boundary = boundary
	o.prepared = true
	o.mu.Unlock()
	return nil
}

// Bind atomically associates the prepared boundary with generation.
func (o *Observer) Bind(ctx context.Context, generation string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if generation == "" {
		return fmt.Errorf("readiness generation is empty: %w", ErrNotPrepared)
	}
	o.mu.Lock()
	prepared, boundary := o.prepared, o.boundary
	o.mu.Unlock()
	if !prepared {
		return ErrNotPrepared
	}
	rec := state.ReadinessRecord{
		Generation: generation,
		Ready:      false,
		Log:        boundary,
	}
	if err := o.store.Save(rec); err != nil {
		return err
	}
	o.mu.Lock()
	o.prepared = false
	o.boundary = nil
	o.mu.Unlock()
	return nil
}

// State returns Running only for matching ready evidence, Starting for matching
// pending evidence without the marker, and Unknown for missing or stale
// evidence. Once ready, log rotation or truncation does not revoke it.
func (o *Observer) State(ctx context.Context, generation string) (minecraft.State, error) {
	if err := ctx.Err(); err != nil {
		return minecraft.Unknown, err
	}
	if generation == "" {
		return minecraft.Unknown, nil
	}
	rec, ok, err := o.store.Load()
	if err != nil {
		return minecraft.Unknown, err
	}
	if !ok || rec.Generation != generation {
		return minecraft.Unknown, nil
	}
	if rec.Ready {
		return minecraft.Running, nil
	}
	found, cursor, err := scan(ctx, o.path, rec.Log)
	if err != nil {
		return minecraft.Unknown, err
	}
	if found {
		rec.Ready = true
		rec.Log = cursor
		if err := o.store.Save(rec); err != nil {
			return minecraft.Unknown, err
		}
		return minecraft.Running, nil
	}
	if cursor != nil && !sameBoundary(cursor, rec.Log) {
		rec.Log = cursor
		if err := o.store.Save(rec); err != nil {
			return minecraft.Unknown, err
		}
	}
	return minecraft.Starting, nil
}

func sameBoundary(a, b *state.LogBoundary) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func fileBoundary(path string) (*state.LogBoundary, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat log %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("log %s is not a regular file", path)
	}
	// Stat alone can succeed on an unreadable file. Open it now so a permission
	// problem is reported before a new generation starts, rather than only when
	// a later status tries to read the log.
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open log %s: %w", path, err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat open log %s: %w", path, err)
	}
	if !opened.Mode().IsRegular() {
		return nil, fmt.Errorf("log %s is not a regular file", path)
	}
	stat, ok := opened.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("log %s has no file identity", path)
	}
	return &state.LogBoundary{
		Device: uint64(stat.Dev),
		Inode:  stat.Ino,
		Offset: opened.Size(),
	}, nil
}

// scan reads complete lines from the boundary onward and reports whether the
// startup marker was found. A partial trailing line is not treated as evidence
// and is not included in the returned cursor, so a marker split across a later
// write is still observed.
func scan(ctx context.Context, path string, boundary *state.LogBoundary) (bool, *state.LogBoundary, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, fmt.Errorf("open log %s: %w", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false, nil, fmt.Errorf("stat open log %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return false, nil, fmt.Errorf("log %s is not a regular file", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, nil, fmt.Errorf("log %s has no file identity", path)
	}
	current := &state.LogBoundary{
		Device: uint64(stat.Dev),
		Inode:  stat.Ino,
		Offset: info.Size(),
	}
	offset := startOffset(boundary, current)
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return false, nil, fmt.Errorf("seek log %s: %w", path, err)
	}

	reader := bufio.NewReaderSize(f, 64*1024)
	position := offset
	for {
		if err := ctx.Err(); err != nil {
			return false, nil, err
		}
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 && err == nil {
			position += int64(len(line))
			if readyLine(line) {
				return true, &state.LogBoundary{Device: current.Device, Inode: current.Inode, Offset: position}, nil
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return false, nil, fmt.Errorf("read log %s: %w", path, err)
		}
	}
	return false, &state.LogBoundary{Device: current.Device, Inode: current.Inode, Offset: position}, nil
}

// startOffset chooses where to resume. A changed inode means rotation or
// recreation and starts at zero; a shrunken file means truncation and also
// starts at zero. Only an unchanged, non-shrunk file resumes at its boundary.
func startOffset(boundary, current *state.LogBoundary) int64 {
	if boundary == nil {
		return 0
	}
	if boundary.Device != current.Device || boundary.Inode != current.Inode {
		return 0
	}
	if current.Offset < boundary.Offset {
		return 0
	}
	return boundary.Offset
}

// readyLine recognizes the vanilla and Forge forms of the startup completion
// line without depending on the exact logger prefix.
func readyLine(line []byte) bool {
	return bytes.Contains(line, []byte("Done (")) &&
		bytes.Contains(line, []byte(`For help, type "help"`))
}
