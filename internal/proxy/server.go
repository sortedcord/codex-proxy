package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/sortedcord/codex-proxy/internal/buildinfo"
	"github.com/sortedcord/codex-proxy/internal/config"
)

func Run(ctx context.Context, cfg config.Config, logger *slog.Logger) (runErr error) {
	if ctx == nil || logger == nil {
		return errors.New("server requires a context and logger")
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}
	defer func() {
		// Serve owns the listener once started; tolerate its prior close.
		if err := ln.Close(); !errors.Is(err, net.ErrClosed) {
			runErr = errors.Join(runErr, err)
		}
	}()
	rt, err := NewRuntime(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, rt.Close()) }()
	return serve(ctx, ln, rt, logger)
}

func serve(ctx context.Context, ln net.Listener, handler http.Handler, logger *slog.Logger) error {
	owned, cancel := context.WithCancel(ctx)
	defer cancel()
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-owned.Done()
		shutdownCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
		}
	}()
	identity := buildinfo.Current()
	logger.Info("listening", "addr", ln.Addr().String(),
		"version", identity.Version, "commit", identity.Commit, "date", identity.Date)
	err := server.Serve(ln)
	// A permanent accept error must wake the shutdown worker before joining it.
	cancel()
	<-shutdownDone
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func newHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.MaxIdleConnsPerHost = 16
	return &http.Client{
		Transport:     transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
}
