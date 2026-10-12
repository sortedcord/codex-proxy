package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sortedcord/codex-proxy/internal/config"
)

type brokenListener struct{ err error }

func (l brokenListener) Accept() (net.Conn, error) { return nil, l.err }
func (brokenListener) Close() error                { return nil }
func (brokenListener) Addr() net.Addr              { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

func TestServeAcceptFailureDoesNotDeadlock(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failure := errors.New("permanent accept failure")
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, brokenListener{err: failure}, http.NotFoundHandler(),
			slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	select {
	case err := <-done:
		if !errors.Is(err, failure) {
			t.Fatalf("serve error = %v, want accept failure", err)
		}
	case <-time.After(time.Second):
		t.Fatal("serve waited for external cancellation after accept failure")
	}
}

func TestServeDrainsOnCancellation(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, ln, http.NotFoundHandler(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get("http://" + ln.Addr().String() + "/health")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server failed to drain")
	}
}

func serverTestConfig(t *testing.T) config.Config {
	t.Helper()
	dir := t.TempDir()
	return config.Config{
		Addr: "127.0.0.1:0", AuthFile: filepath.Join(dir, "credentials", "auth.json"),
		Upstream: config.DefaultUpstream, UsageURL: "https://chatgpt.com/backend-api/wham/usage",
		ClientVersion: config.DefaultClientVersion, UserAgent: "test", IdleTimeout: time.Second,
		HistoryDir: filepath.Join(dir, "history"), HistoryMaxSamples: 5, PollInterval: time.Hour,
	}
}

func TestRunListenFailureDoesNotInitializeRuntime(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cfg := serverTestConfig(t)
	cfg.Addr = ln.Addr().String()
	err = Run(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("occupied listener accepted")
	}
	if _, err := os.Stat(filepath.Dir(cfg.AuthFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("listen failure touched credentials")
	}
}

func TestRunCanceledContextDoesNotInitializeRuntime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Run(ctx, serverTestConfig(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run error=%v, want cancellation", err)
	}
}
