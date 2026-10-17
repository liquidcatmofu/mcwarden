package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
)

func TestExitCode(t *testing.T) {
	for _, tt := range []struct { err error; code int }{
		{nil, 0}, {errors.New("unknown"), 1}, {ErrInvalid, 2}, {ErrNotRunning, 3},
		{ErrTimeout, 4}, {context.DeadlineExceeded, 4}, {ErrUnsupported, 5},
		{os.ErrPermission, 6}, {ErrConfirmation, 9}, {ErrExternal, 10}, {context.Canceled, 130},
	} {
		err := tt.err
		if err != nil { err = fmt.Errorf("operation: %w", err) }
		if code := ExitCode(err); code != tt.code { t.Errorf("ExitCode(%v)=%d want=%d", err, code, tt.code) }
	}
}
