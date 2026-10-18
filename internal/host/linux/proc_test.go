//go:build linux

package linux

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestParseStatHandlesSpacesInCommandName(t *testing.T) {
	fields := make([]string, 20) // stat fields 3..22
	for i := range fields {
		fields[i] = "0"
	}
	fields[0] = "S"          // state
	fields[1] = "100"        // ppid
	fields[2] = "1234"       // pgrp
	fields[19] = "987654321" // starttime

	line := "1234 (my server (v2) main) " + strings.Join(fields, " ")
	got, err := parseStat([]byte(line))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := Process{PID: 1234, PPID: 100, PGID: 1234, State: 'S', StartTime: 987654321}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestParseStatRejectsShortInput(t *testing.T) {
	if _, err := parseStat([]byte("1234 (java) S 1 1")); err == nil {
		t.Fatal("expected error for truncated stat")
	}
}

func TestReadSelf(t *testing.T) {
	got, err := Read(os.Getpid())
	if err != nil {
		t.Fatalf("read self: %v", err)
	}
	if got.PID != os.Getpid() || got.PGID == 0 || got.StartTime == 0 {
		t.Fatalf("unexpected snapshot: %+v", got)
	}
}

func TestReadMissingProcess(t *testing.T) {
	if _, err := Read(1 << 30); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err=%v", err)
	}
}
