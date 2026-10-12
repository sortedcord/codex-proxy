package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	defaultUpstream      = "https://chatgpt.com/backend-api/codex"
	defaultTokenURL      = "https://auth.openai.com/oauth/token"
	defaultClientVersion = "0.162.1"
)

type config struct {
	addr          string
	authFile      string
	apiKey        string
	upstream      string
	clientVersion string
	userAgent     string
	idleTimeout   time.Duration
}

func parseConfig(args []string, output io.Writer) (config, error) {
	cfg := config{}
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return cfg, errors.New("cannot resolve home directory; set CODEX_HOME")
		}
		home = filepath.Join(userHome, ".codex")
	}

	flags := flag.NewFlagSet("codex-proxy", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(
		&cfg.addr,
		"addr",
		envDefault("CODEX_PROXY_ADDR", "127.0.0.1:8080"),
		"HTTP listen address (non-loopback requires CODEX_PROXY_API_KEY)",
	)
	flags.StringVar(
		&cfg.authFile,
		"auth-file",
		envDefault("CODEX_PROXY_AUTH_FILE", filepath.Join(home, "auth.json")),
		"Codex ChatGPT auth.json file",
	)
	flags.StringVar(
		&cfg.upstream,
		"upstream",
		envDefault("CODEX_PROXY_UPSTREAM", defaultUpstream),
		"trusted Codex backend base URL (receives your access token)",
	)
	flags.StringVar(
		&cfg.clientVersion,
		"client-version",
		envDefault("CODEX_PROXY_CLIENT_VERSION", defaultClientVersion),
		"Codex client version for model discovery and upstream headers",
	)
	flags.DurationVar(
		&cfg.idleTimeout,
		"idle-timeout",
		5*time.Minute,
		"upstream body idle timeout",
	)
	if err := flags.Parse(args); err != nil {
		return cfg, err
	}
	if flags.NArg() != 0 {
		return cfg, errors.New("unexpected positional arguments")
	}

	cfg.apiKey = os.Getenv("CODEX_PROXY_API_KEY")
	cfg.userAgent = envDefault(
		"CODEX_PROXY_USER_AGENT",
		"codex_cli_rs/"+cfg.clientVersion+" ("+runtime.GOOS+"; "+runtime.GOARCH+")",
	)
	cfg.upstream = strings.TrimRight(cfg.upstream, "/")
	return cfg, cfg.validate()
}

func (c config) validate() error {
	host, _, err := net.SplitHostPort(c.addr)
	if err != nil {
		return fmt.Errorf("invalid listen address: %w", err)
	}
	if !isLoopback(host) && c.apiKey == "" {
		return errors.New("non-loopback listening requires CODEX_PROXY_API_KEY")
	}
	if c.authFile == "" {
		return errors.New("auth file path is required")
	}
	if c.idleTimeout <= 0 {
		return errors.New("idle timeout must be positive")
	}
	if c.clientVersion == "" || strings.ContainsAny(c.clientVersion+c.userAgent+c.apiKey, "\r\n") {
		return errors.New("invalid client version, user agent, or proxy API key")
	}

	u, err := url.Parse(c.upstream)
	if err != nil {
		return errors.New("invalid upstream URL")
	}
	invalidURL := u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != ""
	secureTransport := u.Scheme == "https" || (u.Scheme == "http" && isLoopback(u.Hostname()))
	if invalidURL || !secureTransport {
		return errors.New("upstream must be HTTPS (or loopback HTTP), without credentials, query, or fragment")
	}
	return nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func envDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
