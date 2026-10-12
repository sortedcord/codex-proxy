package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/sortedcord/codex-proxy/internal/storage"
)

type listenHandler struct{ ready chan string }

func (listenHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h listenHandler) Handle(_ context.Context, record slog.Record) error {
	if record.Message == "listening" {
		record.Attrs(func(attr slog.Attr) bool {
			if attr.Key == "addr" {
				h.ready <- attr.Value.String()
			}
			return true
		})
	}
	return nil
}
func (h listenHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h listenHandler) WithGroup(string) slog.Handler      { return h }

func TestRunHealthyStartupShutdownAndLockRelease(t *testing.T) {
	cfg := serverTestConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, slog.New(listenHandler{ready: ready})) }()
	var address string
	select {
	case address = <-ready:
	case err := <-done:
		t.Fatalf("runtime exited before listening: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("runtime failed to listen")
	}
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get("http://" + address + "/health")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", resp.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runtime did not shut down")
	}
	lock, err := storage.Acquire(cfg.AuthFile + ".codex-proxy.lock")
	if err != nil {
		t.Fatalf("runtime leaked its credential lock: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}
