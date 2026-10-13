package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/sortedcord/codex-proxy/internal/buildinfo"
)

func TestVersionDoesNotInitializeRuntime(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_PROXY_AUTH_FILE", filepath.Join(dir, "auth.json"))
	t.Setenv("CODEX_PROXY_HISTORY_DIR", filepath.Join(dir, "history"))
	// Invalid serving configuration must not affect version dispatch.
	t.Setenv("CODEX_PROXY_QUOTA_POLL_INTERVAL", "invalid")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var output, diagnostics bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&diagnostics, nil))
	if err := runContext(ctx, []string{"version"}, logger, &output, &diagnostics); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), buildinfo.Current().String()+"\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if diagnostics.Len() != 0 {
		t.Fatalf("unexpected diagnostics: %s", &diagnostics)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("version initialized state: entries=%v, error=%v", entries, err)
	}
}

func TestVersionRejectsArguments(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := run([]string{"version", "serve"}, logger, &output, io.Discard); err == nil {
		t.Fatal("expected usage error")
	}
	if output.Len() != 0 {
		t.Fatal("unexpected version output")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestVersionPropagatesOutputError(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := run([]string{"version"}, logger, failingWriter{}, io.Discard); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("error = %v, want closed pipe", err)
	}
}
