package app

import (
	"context"
	"errors"
	"os"
)

var (
	ErrStartupFailed = errors.New("server exited during startup")
	ErrInvalid       = errors.New("invalid options")
	ErrConfig        = errors.New("configuration error")
	ErrNotRunning    = errors.New("server not running")
	ErrUnsupported   = errors.New("unsupported capability")
	ErrTimeout       = errors.New("operation timed out")
	ErrConfirmation  = errors.New("confirmation required")
	ErrExternal      = errors.New("external tool failed")
)

// ExitCode is shared by text and JSON CLI output.
func ExitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ErrInvalid), errors.Is(err, ErrConfig):
		return 2
	case errors.Is(err, ErrNotRunning):
		return 3
	case errors.Is(err, ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return 4
	case errors.Is(err, ErrUnsupported):
		return 5
	case errors.Is(err, os.ErrPermission):
		return 6
	case errors.Is(err, ErrConfirmation):
		return 9
	case errors.Is(err, ErrExternal):
		return 10
	case errors.Is(err, context.Canceled):
		return 130
	default:
		return 1
	}
}
