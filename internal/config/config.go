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
)

const (
	DefaultUpstream      = "https://chatgpt.com/backend-api/codex"
	DefaultTokenURL      = "https://auth.openai.com/oauth/token"
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

func Parse(args []string, output io.Writer) (Config, error) {
	cfg := Config{}
	home := os.Getenv("CODEX_HOME")
	userHome, err := os.UserHomeDir()
	if err != nil && home == "" {
		return cfg, errors.New("cannot resolve home directory; set CODEX_HOME")
	}
	if home == "" {
		home = filepath.Join(userHome, ".codex")
	}
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		dataHome = filepath.Join(userHome, ".local", "share")
	}
	flags := flag.NewFlagSet("codex-proxy serve", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(
		&cfg.Addr,
		"addr",
		envDefault("CODEX_PROXY_ADDR", "127.0.0.1:8080"),
		"HTTP listen address (non-loopback requires CODEX_PROXY_API_KEY)",
	)
	flags.StringVar(
		&cfg.AuthFile,
		"auth-file",
		envDefault("CODEX_PROXY_AUTH_FILE", filepath.Join(home, "auth.json")),
		"Codex ChatGPT auth.json file (writable for refresh and runtime login)",
	)
	flags.StringVar(
		&cfg.Upstream,
		"upstream",
		envDefault("CODEX_PROXY_UPSTREAM", DefaultUpstream),
		"trusted Codex backend base URL (receives your access token)",
	)
	flags.StringVar(
		&cfg.ClientVersion,
		"client-version",
		envDefault("CODEX_PROXY_CLIENT_VERSION", DefaultClientVersion),
		"Codex client version for model discovery and upstream headers",
	)
	flags.StringVar(
		&cfg.UsageURL,
		"usage-url",
		os.Getenv("CODEX_PROXY_USAGE_URL"),
		"trusted Codex account quota endpoint (receives your access token)",
	)
	flags.StringVar(
		&cfg.HistoryDir,
		"history-dir",
		envDefault("CODEX_PROXY_HISTORY_DIR", filepath.Join(dataHome, "codex-proxy")),
		"private directory for account-specific quota history",
	)
	flags.DurationVar(
		&cfg.IdleTimeout,
		"idle-timeout",
		5*time.Minute,
		"upstream body idle timeout",
	)
	pollInterval, err := time.ParseDuration(envDefault("CODEX_PROXY_QUOTA_POLL_INTERVAL", "5m"))
	if err != nil {
		return cfg, errors.New("invalid CODEX_PROXY_QUOTA_POLL_INTERVAL")
	}
	flags.DurationVar(
		&cfg.PollInterval,
		"quota-poll-interval",
		pollInterval,
		"quota polling interval (10s to 24h)",
	)
	flags.IntVar(
		&cfg.HistoryMaxSamples,
		"history-max-samples",
		10000,
		"maximum retained quota observations per account (1 to 10000)",
	)
	if err := flags.Parse(args); err != nil {
		return cfg, err
	}
	if flags.NArg() != 0 {
		return cfg, errors.New("unexpected positional arguments")
	}
	cfg.APIKey = os.Getenv("CODEX_PROXY_API_KEY")
	cfg.ManagementKey = os.Getenv("CODEX_PROXY_MANAGEMENT_KEY")
	cfg.UserAgent = envDefault(
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
	if c.HistoryMaxSamples < 1 || c.HistoryMaxSamples > 10000 {
		return errors.New("history sample limit must be between 1 and 10000")
	}
	invalidHeader := strings.ContainsAny(c.ClientVersion+c.UserAgent+c.APIKey+c.ManagementKey, "\r\n")
	if c.ClientVersion == "" || invalidHeader {
		return errors.New("invalid client version, user agent, or local key")
	}
	if c.ManagementKey != "" && c.ManagementKey == c.APIKey {
		return errors.New("management and inference keys must be different")
	}
	for _, endpoint := range []string{c.Upstream, c.UsageURL} {
		if err := ValidateEndpoint(endpoint); err != nil {
			return err
		}
	}
	return nil
}

func ValidateEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return errors.New("invalid upstream URL")
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

func envDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
