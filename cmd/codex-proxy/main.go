package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/sortedcord/codex-proxy/internal/buildinfo"
	"github.com/sortedcord/codex-proxy/internal/config"
	"github.com/sortedcord/codex-proxy/internal/proxy"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	err := run(os.Args[1:], logger, os.Stdout, os.Stderr)
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return
	}
	logger.Error("proxy stopped", "error", err)
	os.Exit(1)
}

func run(args []string, logger *slog.Logger, output, errOutput io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runContext(ctx, args, logger, output, errOutput)
}

func runContext(ctx context.Context, args []string, logger *slog.Logger, output, errOutput io.Writer) error {
	if len(args) > 0 && args[0] == "version" {
		if len(args) != 1 {
			return errors.New("usage: codex-proxy version")
		}
		_, err := fmt.Fprintln(output, buildinfo.Current())
		return err
	}
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	cfg, err := config.Parse(args, errOutput)
	if err != nil {
		return err
	}
	return proxy.Run(ctx, cfg, logger)
}
