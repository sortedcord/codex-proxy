// Package config defines operator configuration; credentials stay in Codex's auth file.
package config

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

	"github.com/sortedcord/codex-proxy/internal/storage"
)

const (
	DefaultUpstream      = "https://chatgpt.com/backend-api/codex"
	DefaultClientVersion = "0.162.1"
)

type Config struct {
	Addr              string
	AuthFile          string
	APIKey            string
	ManagementKey     string
	Upstream          string
	ClientVersion     string
	UserAgent         string
	IdleTimeout       time.Duration
	UsageURL          string
	HistoryDir        string
	PollInterval      time.Duration
	HistoryMaxSamples int
}

type environment struct {
	lookup func(string) (string, bool)
	home   func() (string, error)
}

func (e environment) value(name string) string {
	value, _ := e.lookup(name)
	return value
}

func (e environment) defaultValue(name, fallback string) string {
	if value := e.value(name); value != "" {
		return value
	}
	return fallback
}

func Parse(args []string, output io.Writer) (Config, error) {
	return parse(args, output, environment{lookup: os.LookupEnv, home: os.UserHomeDir})
}

func parse(args []string, output io.Writer, env environment) (Config, error) {
	var cfg Config
	authFile, historyDir := defaultPaths(env)
	flags := flag.NewFlagSet("codex-proxy serve", flag.ContinueOnError)
	flags.SetOutput(output)
	options := []struct {
		value    *string
		name     string
		fallback string
		help     string
	}{
		{
			value: &cfg.Addr, name: "addr", fallback: env.defaultValue("CODEX_PROXY_ADDR", "127.0.0.1:8080"),
			help: "HTTP listen address (non-loopback requires CODEX_PROXY_API_KEY)",
		},
		{
			value: &cfg.AuthFile, name: "auth-file", fallback: env.defaultValue("CODEX_PROXY_AUTH_FILE", authFile),
			help: "Codex ChatGPT auth.json file (writable for refresh and runtime login)",
		},
		{
			value: &cfg.Upstream, name: "upstream", fallback: env.defaultValue("CODEX_PROXY_UPSTREAM", DefaultUpstream),
			help: "trusted Codex backend base URL (receives your access token)",
		},
		{
			value: &cfg.ClientVersion, name: "client-version",
			fallback: env.defaultValue("CODEX_PROXY_CLIENT_VERSION", DefaultClientVersion),
			help:     "Codex client version for model discovery and upstream headers",
		},
		{
			value: &cfg.UsageURL, name: "usage-url", fallback: env.value("CODEX_PROXY_USAGE_URL"),
			help: "trusted Codex account quota endpoint (receives your access token)",
		},
		{
			value: &cfg.HistoryDir, name: "history-dir", fallback: env.defaultValue("CODEX_PROXY_HISTORY_DIR", historyDir),
			help: "private directory for account-specific quota history",
		},
	}
	for _, option := range options {
		flags.StringVar(
			option.value,
			option.name,
			option.fallback,
			option.help,
		)
	}
	flags.DurationVar(
		&cfg.IdleTimeout,
		"idle-timeout",
		5*time.Minute,
		"upstream body idle timeout",
	)
	pollInterval := env.defaultValue("CODEX_PROXY_QUOTA_POLL_INTERVAL", "5m")
	flags.StringVar(
		&pollInterval,
		"quota-poll-interval",
		pollInterval,
		"quota polling interval (10s to 24h)",
	)
	flags.IntVar(
		&cfg.HistoryMaxSamples,
		"history-max-samples",
		storage.MaxHistorySamples,
		"maximum retained quota observations per account (1 to 10000)",
	)
	if err := flags.Parse(args); err != nil {
		return cfg, err
	}
	if flags.NArg() != 0 {
		return cfg, errors.New("unexpected positional arguments")
	}
	if cfg.AuthFile == "" {
		return cfg, errors.New("cannot resolve auth file; set CODEX_HOME or -auth-file")
	}
	if cfg.HistoryDir == "" {
		return cfg, errors.New("cannot resolve history directory; set XDG_DATA_HOME or -history-dir")
	}
	parsedInterval, err := time.ParseDuration(pollInterval)
	if err != nil {
		return cfg, errors.New("invalid quota polling interval (-quota-poll-interval or CODEX_PROXY_QUOTA_POLL_INTERVAL)")
	}
	cfg.PollInterval = parsedInterval
	cfg.APIKey = env.value("CODEX_PROXY_API_KEY")
	cfg.ManagementKey = env.value("CODEX_PROXY_MANAGEMENT_KEY")
	cfg.UserAgent = env.defaultValue(
		"CODEX_PROXY_USER_AGENT",
		"codex_cli_rs/"+cfg.ClientVersion+" ("+runtime.GOOS+"; "+runtime.GOARCH+")",
	)
	cfg.Upstream = strings.TrimRight(cfg.Upstream, "/")
	if cfg.UsageURL == "" {
		cfg.UsageURL = cfg.Upstream + "/usage"
		if strings.HasSuffix(cfg.Upstream, "/codex") {
			cfg.UsageURL = strings.TrimSuffix(cfg.Upstream, "/codex") + "/wham/usage"
		}
	}
	return cfg, cfg.Validate()
}

func defaultPaths(env environment) (string, string) {
	home := env.value("CODEX_HOME")
	userHome, _ := env.home()
	if home == "" && userHome != "" {
		home = filepath.Join(userHome, ".codex")
	}
	var authFile string
	if home != "" {
		authFile = filepath.Join(home, "auth.json")
	}
	dataHome := env.value("XDG_DATA_HOME")
	if dataHome == "" && userHome != "" {
		dataHome = filepath.Join(userHome, ".local", "share")
	}
	var historyDir string
	if dataHome != "" {
		historyDir = filepath.Join(dataHome, "codex-proxy")
	}
	return authFile, historyDir
}

func (c Config) Validate() error {
	host, _, err := net.SplitHostPort(c.Addr)
	if err != nil {
		return fmt.Errorf("invalid listen address: %w", err)
	}
	if !isLoopback(host) && c.APIKey == "" {
		return errors.New("non-loopback listening requires CODEX_PROXY_API_KEY")
	}
	if c.AuthFile == "" || strings.TrimSpace(c.HistoryDir) == "" {
		return errors.New("auth file and history directory are required")
	}
	if c.IdleTimeout <= 0 {
		return errors.New("idle timeout must be positive")
	}
	if c.PollInterval < 10*time.Second || c.PollInterval > 24*time.Hour {
		return errors.New("quota polling interval must be between 10s and 24h")
	}
	if c.HistoryMaxSamples < 1 || c.HistoryMaxSamples > storage.MaxHistorySamples {
		return errors.New("history sample limit must be between 1 and 10000")
	}
	invalidHeader := strings.ContainsAny(c.ClientVersion+c.UserAgent+c.APIKey+c.ManagementKey, "\r\n")
	if c.ClientVersion == "" || invalidHeader {
		return errors.New("invalid client version, user agent, or local key")
	}
	if c.ManagementKey != "" && c.ManagementKey == c.APIKey {
		return errors.New("management and inference keys must be different")
	}
	if err := ValidateEndpoint(c.Upstream); err != nil {
		return fmt.Errorf("invalid upstream: %w", err)
	}
	if err := ValidateEndpoint(c.UsageURL); err != nil {
		return fmt.Errorf("invalid usage endpoint: %w", err)
	}
	return nil
}

func ValidateEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		// Parse errors may echo credentials or query secrets; never wrap them.
		return errors.New("invalid endpoint URL")
	}
	invalidURL := u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != ""
	secureTransport := u.Scheme == "https" || (u.Scheme == "http" && isLoopback(u.Hostname()))
	if invalidURL || !secureTransport {
		return errors.New("endpoint must be HTTPS (or loopback HTTP), without credentials, query, or fragment")
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
