package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("proxy stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := parseConfig(os.Args[1:], os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}

	client := newHTTPClient()
	defer client.CloseIdleConnections()
	auth, err := newAuthManager(cfg.authFile, client, defaultTokenURL)
	if err != nil {
		return err
	}
	proxy := newProxy(
		cfg,
		auth,
		client,
		logger,
	)
	server := &http.Server{
		Addr:              cfg.addr,
		Handler:           proxy,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
		// Stream writes use per-write deadlines, not a whole-response timeout.
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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

	logger.Info(
		"listening",
		"addr",
		cfg.addr,
		"auth_file",
		cfg.authFile,
	)
	err = server.ListenAndServe()
	stop()
	<-shutdownDone
	auth.waitForRefresh()
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
		Transport: transport,
		// Never follow redirects carrying account credentials.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
