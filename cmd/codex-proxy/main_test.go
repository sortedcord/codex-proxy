package main

import (
	"errors"
	"flag"
	"io"
	"log/slog"
	"testing"
)

func TestRunHelpAndInvalidFlags(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	err := run([]string{"-help"}, logger, io.Discard)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("expected flag.ErrHelp, got %v", err)
	}

	err = run([]string{"serve", "-help"}, logger, io.Discard)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("expected flag.ErrHelp with serve prefix, got %v", err)
	}

	err = run([]string{"-unknown-flag"}, logger, io.Discard)
	if err == nil {
		t.Fatal("expected error on unknown flag")
	}
}
