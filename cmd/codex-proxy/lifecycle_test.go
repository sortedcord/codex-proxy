package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
)

func TestCLIValidServeHonorsCancellation(t *testing.T) {
	t.Setenv("CODEX_PROXY_API_KEY", "")
	t.Setenv("CODEX_PROXY_MANAGEMENT_KEY", "")
	t.Setenv("CODEX_PROXY_QUOTA_POLL_INTERVAL", "5m")
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runContext(ctx, []string{
		"serve", "-addr", "127.0.0.1:0",
		"-auth-file", filepath.Join(dir, "auth.json"),
		"-history-dir", filepath.Join(dir, "history"),
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), io.Discard, io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("valid serve command = %v, want cancellation", err)
	}
}
