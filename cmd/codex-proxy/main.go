package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/sortedcord/codex-proxy/internal/config"
	"github.com/sortedcord/codex-proxy/internal/proxy"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	err := run(os.Args[1:], logger, os.Stderr)
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return
	}
	logger.Error("proxy stopped", "error", err)
	os.Exit(1)
}

func run(args []string, logger *slog.Logger, errOutput io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runContext(ctx, args, logger, errOutput)
}

func runContext(ctx context.Context, args []string, logger *slog.Logger, errOutput io.Writer) error {
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	cfg, err := config.Parse(args, errOutput)
	if err != nil {
		return err
	}
	return proxy.Run(ctx, cfg, logger)
}
