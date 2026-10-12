package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/sortedcord/codex-proxy/internal/config"
)

func Run(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	runtime, err := NewRuntime(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer runtime.Close()
	server := &http.Server{
		Addr: cfg.Addr, Handler: runtime, ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 64 << 10,
	}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
		}
	}()
	defer func() { cancel(); <-shutdownDone }()
	logger.Info("listening", "addr", cfg.Addr)
	err = server.ListenAndServe()
	// A listen failure must also release the shutdown waiter.
	if !errors.Is(err, http.ErrServerClosed) {
		_ = server.Close()
		return err
	}
	<-shutdownDone
	return nil
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
