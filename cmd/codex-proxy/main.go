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
	"strings"
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
	if len(args) > 0 && (args[0] == "help" || args[0] == "--help") {
		variables := config.EnvironmentVariables()
		_, err := fmt.Fprintf(output, "Codex Proxy\n\nUsage:\n  codex-proxy [serve]\n  codex-proxy login [--account ID] [--replace] [--issue-key LABEL]\n  codex-proxy accounts <list|enable|disable|remove> [ID]\n  codex-proxy keys <list|create|revoke> <ACCOUNT_ID> [KEY_ID] [--label LABEL]\n  codex-proxy admin-key rotate\n  codex-proxy version\n\nEnvironment:\n  %s\n", strings.Join(variables, "\n  "))
		return err
	}
	if len(args) > 0 {
		switch args[0] {
		case "login", "accounts", "keys", "admin-key":
			cfg, path, err := config.Load()
			if err != nil {
				return err
			}
			switch args[0] {
			case "login":
				return runLoginCommand(args[1:], cfg, path, output)
			case "accounts":
				return runAccountsCommand(args[1:], cfg, path, output)
			case "keys":
				return runKeysCommand(args[1:], cfg, path, output)
			case "admin-key":
				return runAdminKeyCommand(args[1:], path, output)
			}
		case "serve":
			args = args[1:]
		}
	}
	cfg, err := config.Parse(args, errOutput)
	if err != nil {
		return err
	}
	return proxy.Run(ctx, cfg, logger)
}
