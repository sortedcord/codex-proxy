// Package config defines operator configuration; credentials stay in Codex's auth file or config.json.
package config

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
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

	"github.com/sortedcord/codex-proxy/internal/apikey"
	"github.com/sortedcord/codex-proxy/internal/storage"
	"github.com/terastal-hq/proxylib/privatefile"
)

const (
	DefaultUpstream      = "https://chatgpt.com/backend-api/codex"
	DefaultClientVersion = "0.162.1"
)

type Config struct {
	Addr          string        `json:"addr,omitempty"`
	AuthFile      string        `json:"authFile,omitempty"`
	ConfigFile    string        `json:"configFile,omitempty"`
	ConfigPath    string        `json:"-"`
	APIKey        string        `json:"apiKey,omitempty"`
	ManagementKey string        `json:"managementKey,omitempty"`
	AdminKeyHash  string        `json:"adminKeyHash,omitempty"`
	AdminAPIKey   string        `json:"-"`
	Accounts      []Account     `json:"accounts,omitempty"`
	Upstream      string        `json:"upstream,omitempty"`
	ClientVersion string        `json:"clientVersion,omitempty"`
	UserAgent     string        `json:"userAgent,omitempty"`
	IdleTimeout   time.Duration `json:"idleTimeout,omitempty"`
	UsageURL      string        `json:"usageUrl,omitempty"`
	// Deprecated: quota history is no longer persisted. HistoryDir and
	// HistoryMaxSamples are accepted so existing configs and flags still load,
	// but they are ignored.
	HistoryDir        string        `json:"historyDir,omitempty"`
	PollInterval      time.Duration `json:"pollInterval,omitempty"`
	HistoryMaxSamples int           `json:"historyMaxSamples,omitempty"`

	// Legacy fields for backward-compatible disk loading and migration
	AccessToken  string `json:"accessToken,omitempty"`
	RefreshToken string `json:"refreshToken,omitempty"`
	IDToken      string `json:"idToken,omitempty"`
	AccountID    string `json:"accountId,omitempty"`
	AccountEmail string `json:"accountEmail,omitempty"`
	AccountName  string `json:"accountName,omitempty"`
	PlanType     string `json:"planType,omitempty"`
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

// EnvironmentVariables returns all canonical environment variables.
func EnvironmentVariables() []string {
	return []string{
		"CODEX_PROXY_ADDR",
		"CODEX_PROXY_AUTH_FILE",
		"CODEX_PROXY_CONFIG_FILE",
		"CODEX_PROXY_API_KEY",
		"CODEX_PROXY_MANAGEMENT_KEY",
		"ADMIN_API_KEY",
		"API_KEY",
		"CODEX_PROXY_UPSTREAM",
		"CODEX_PROXY_CLIENT_VERSION",
		"CODEX_PROXY_USER_AGENT",
		"CODEX_PROXY_USAGE_URL",
		"CODEX_PROXY_HISTORY_DIR",
		"CODEX_PROXY_QUOTA_POLL_INTERVAL",
	}
}

// Load reads the default config.json, applies defaults and environment overrides.
func Load() (Config, string, error) {
	return ParseWithArgs(nil, io.Discard)
}

// Parse parses CLI arguments, environment, and disk configuration.
func Parse(args []string, output io.Writer) (Config, error) {
	return parse(args, output, environment{lookup: os.LookupEnv, home: os.UserHomeDir})
}

// ParseWithArgs parses CLI arguments, environment, and disk configuration, returning the config and path.
func ParseWithArgs(args []string, output io.Writer) (Config, string, error) {
	cfg, err := parse(args, output, environment{lookup: os.LookupEnv, home: os.UserHomeDir})
	return cfg, cfg.ConfigPath, err
}

func parse(args []string, output io.Writer, env environment) (Config, error) {
	var cfg Config
	authFile, historyDir, defaultConfigFile := defaultPaths(env)

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
			help: "HTTP listen address (non-loopback requires CODEX_PROXY_API_KEY or ADMIN_API_KEY)",
		},
		{
			value: &cfg.AuthFile, name: "auth-file", fallback: env.defaultValue("CODEX_PROXY_AUTH_FILE", authFile),
			help: "Codex ChatGPT auth.json file (legacy fallback)",
		},
		{
			value: &cfg.ConfigFile, name: "config", fallback: env.defaultValue("CODEX_PROXY_CONFIG_FILE", defaultConfigFile),
			help: "Codex proxy configuration and accounts file (default: ~/.config/codex-proxy/config.json)",
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
			help: "deprecated and ignored: quota history is no longer persisted",
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
		"deprecated and ignored: quota history is no longer persisted",
	)
	if err := flags.Parse(args); err != nil {
		return cfg, err
	}
	if flags.NArg() != 0 {
		return cfg, errors.New("unexpected positional arguments")
	}

	// 1. Read config from configFile if exists
	configPath := cfg.ConfigFile
	var diskData []byte
	var diskCfg Config
	if configPath != "" {
		var readErr error
		diskData, readErr = readConfigFile(configPath, &diskCfg)
		if readErr != nil && !os.IsNotExist(readErr) {
			return cfg, readErr
		}
		if len(diskData) > 0 {
			cfg.Accounts = diskCfg.Accounts
			cfg.AdminKeyHash = diskCfg.AdminKeyHash
			if cfg.Addr == "127.0.0.1:8080" && diskCfg.Addr != "" {
				cfg.Addr = diskCfg.Addr
			}
			if cfg.Upstream == DefaultUpstream && diskCfg.Upstream != "" {
				cfg.Upstream = diskCfg.Upstream
			}
			if cfg.ClientVersion == DefaultClientVersion && diskCfg.ClientVersion != "" {
				cfg.ClientVersion = diskCfg.ClientVersion
			}
			if diskCfg.APIKey != "" && cfg.APIKey == "" {
				cfg.APIKey = diskCfg.APIKey
			}
			if diskCfg.ManagementKey != "" && cfg.ManagementKey == "" {
				cfg.ManagementKey = diskCfg.ManagementKey
			}
		}
	}

	// 2. If no accounts yet, check if legacy auth.json file exists and migrate automatically
	if len(cfg.Accounts) == 0 && cfg.AuthFile != "" {
		if legacyAcc, err := loadLegacyAuthFile(cfg.AuthFile); err == nil && legacyAcc != nil {
			cfg.Accounts = []Account{*legacyAcc}
		}
	}

	// 3. Environment overrides
	if host := env.value("HOST"); host != "" {
		port := env.defaultValue("PORT", "8080")
		cfg.Addr = net.JoinHostPort(host, port)
	} else if port := env.value("PORT"); port != "" {
		h, _, _ := net.SplitHostPort(cfg.Addr)
		if h == "" {
			h = "127.0.0.1"
		}
		cfg.Addr = net.JoinHostPort(h, port)
	}

	if val := env.value("CODEX_PROXY_API_KEY"); val != "" {
		cfg.APIKey = val
	} else if val := env.value("API_KEY"); val != "" {
		cfg.APIKey = val
	}

	if val := env.value("CODEX_PROXY_MANAGEMENT_KEY"); val != "" {
		cfg.ManagementKey = val
		cfg.AdminAPIKey = val
	}
	if val := env.value("ADMIN_API_KEY"); val != "" {
		cfg.AdminAPIKey = val
		if cfg.ManagementKey == "" {
			cfg.ManagementKey = val
		}
	}

	cfg.ConfigPath = configPath

	if cfg.AuthFile == "" {
		return cfg, errors.New("cannot resolve auth file; set CODEX_HOME or -auth-file")
	}
	parsedInterval, err := time.ParseDuration(pollInterval)
	if err != nil {
		return cfg, errors.New("invalid quota polling interval (-quota-poll-interval or CODEX_PROXY_QUOTA_POLL_INTERVAL)")
	}
	cfg.PollInterval = parsedInterval
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

	if cfg.APIKey != "" {
		cfg.EnsureDefaultAccountKey(cfg.APIKey)
	}

	return cfg, cfg.Validate()
}

func defaultPaths(env environment) (string, string, string) {
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
	configHome := env.value("XDG_CONFIG_HOME")
	if configHome == "" && userHome != "" {
		configHome = filepath.Join(userHome, ".config")
	}
	var configFile string
	if configHome != "" {
		configFile = filepath.Join(configHome, "codex-proxy", "config.json")
	}
	return authFile, historyDir, configFile
}

func (c *Config) normalize() {
	env := environment{lookup: os.LookupEnv, home: os.UserHomeDir}
	authFile, historyDir, configFile := defaultPaths(env)
	if c.AuthFile == "" {
		c.AuthFile = authFile
	}
	if c.HistoryDir == "" {
		c.HistoryDir = historyDir
	}
	if c.ConfigFile == "" {
		c.ConfigFile = configFile
	}
	if c.Addr == "" {
		c.Addr = "127.0.0.1:8080"
	}
	if c.Upstream == "" {
		c.Upstream = DefaultUpstream
	}
	c.Upstream = strings.TrimRight(c.Upstream, "/")
	if c.ClientVersion == "" {
		c.ClientVersion = DefaultClientVersion
	}
	if c.UserAgent == "" {
		c.UserAgent = "codex_cli_rs/" + c.ClientVersion + " (" + runtime.GOOS + "; " + runtime.GOARCH + ")"
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = 5 * time.Minute
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 5 * time.Minute
	}
	if c.HistoryMaxSamples <= 0 {
		c.HistoryMaxSamples = storage.MaxHistorySamples
	}
	if c.UsageURL == "" {
		c.UsageURL = c.Upstream + "/usage"
		if strings.HasSuffix(c.Upstream, "/codex") {
			c.UsageURL = strings.TrimSuffix(c.Upstream, "/codex") + "/wham/usage"
		}
	}
}

func (c Config) hasLegacyCredentials() bool {
	return c.AccessToken != "" || c.RefreshToken != "" || c.AccountID != "" ||
		c.AccountEmail != "" || c.APIKey != ""
}

// HasLegacyCredentials reports whether flat legacy credential fields are present.
func (c Config) HasLegacyCredentials() bool {
	return c.hasLegacyCredentials()
}

func (c Config) migratedLegacyAccounts() []Account {
	var keys []AccountKey
	if c.APIKey != "" {
		keys = append(keys, AccountKey{
			ID:        "key_legacy",
			Label:     "legacy",
			Hash:      apikey.Hash(c.APIKey),
			Last4:     apikey.Last4(c.APIKey),
			CreatedAt: time.Now().UTC(),
		})
	}
	return []Account{
		{
			ID:           "default",
			Label:        "Default",
			Enabled:      true,
			RefreshToken: c.RefreshToken,
			AccessToken:  c.AccessToken,
			IDToken:      c.IDToken,
			AccountID:    c.AccountID,
			AccountEmail: c.AccountEmail,
			AccountName:  c.AccountName,
			PlanType:     c.PlanType,
			Keys:         keys,
		},
	}
}

func (c Config) allKeysCount() int {
	count := 0
	for _, acc := range c.Accounts {
		count += len(acc.Keys)
	}
	return count
}

func (c Config) effectiveListenKey() string {
	if c.APIKey != "" {
		return c.APIKey
	}
	if c.AdminAPIKey != "" {
		return c.AdminAPIKey
	}
	if c.ManagementKey != "" {
		return c.ManagementKey
	}
	if c.AdminKeyHash != "" {
		return c.AdminKeyHash
	}
	if c.allKeysCount() > 0 {
		return "configured"
	}
	return ""
}

// EnsureDefaultAccountKey adds key_env with key's hash to the default account.
func (c *Config) EnsureDefaultAccountKey(key string) {
	if key == "" {
		return
	}
	keyHash := apikey.Hash(key)
	for i := range c.Accounts {
		if c.Accounts[i].ID == "default" {
			for _, k := range c.Accounts[i].Keys {
				if k.Hash == keyHash {
					return
				}
			}
			c.Accounts[i].Keys = append(c.Accounts[i].Keys, AccountKey{
				ID:        "key_env",
				Label:     "env",
				Hash:      keyHash,
				Last4:     apikey.Last4(key),
				CreatedAt: time.Now().UTC(),
			})
			return
		}
	}
	// Append default account if not present
	c.Accounts = append(c.Accounts, Account{
		ID:      "default",
		Label:   "Default",
		Enabled: true,
		Keys: []AccountKey{
			{
				ID:        "key_env",
				Label:     "env",
				Hash:      keyHash,
				Last4:     apikey.Last4(key),
				CreatedAt: time.Now().UTC(),
			},
		},
	})
}

func (c Config) Validate() error {
	host, _, err := net.SplitHostPort(c.Addr)
	if err != nil {
		return fmt.Errorf("invalid listen address: %w", err)
	}
	if !isLoopback(host) && c.effectiveListenKey() == "" {
		return errors.New("non-loopback listening requires CODEX_PROXY_API_KEY")
	}
	if isLoopback(host) && len(c.Accounts) > 1 && c.allKeysCount() == 0 {
		return fmt.Errorf("API keys are required when multiple accounts are configured")
	}
	if err := ValidateAccounts(c.Accounts); err != nil {
		return err
	}
	if c.AuthFile == "" && len(c.Accounts) == 0 {
		return errors.New("auth file or configured accounts are required")
	}
	if c.IdleTimeout <= 0 {
		return errors.New("idle timeout must be positive")
	}
	if c.PollInterval < 10*time.Second || c.PollInterval > 24*time.Hour {
		return errors.New("quota polling interval must be between 10s and 24h")
	}
	invalidHeader := strings.ContainsAny(c.ClientVersion+c.UserAgent+c.APIKey+c.ManagementKey+c.AdminAPIKey, "\r\n")
	if c.ClientVersion == "" || invalidHeader {
		return errors.New("invalid client version, user agent, or local key")
	}
	if c.ManagementKey != "" && c.ManagementKey == c.APIKey {
		return errors.New("management and inference keys must be different")
	}
	if c.AdminAPIKey != "" && c.AdminAPIKey == c.APIKey {
		return errors.New("admin and inference keys must be different")
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

func readConfigFile(path string, cfg *Config) ([]byte, error) {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat config %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("config %s must be a regular file", path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("config %s must have owner-only permissions (chmod 600)", path)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, fmt.Errorf("config %s must contain a JSON object", path)
	}
	if err := rejectDuplicateConfigFields(data); err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(cfg); err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("config %s must contain exactly one JSON object", path)
	}
	if len(cfg.Accounts) > 0 && cfg.hasLegacyCredentials() {
		return nil, fmt.Errorf("config %s cannot mix legacy credential fields with 'accounts' array", path)
	}
	if len(cfg.Accounts) == 0 && cfg.hasLegacyCredentials() {
		cfg.Accounts = cfg.migratedLegacyAccounts()
	}
	return data, nil
}

func saveConfigBytes(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	return storage.AtomicWrite(path, data)
}

func rejectDuplicateConfigFields(data []byte) error {
	return privatefile.RejectDuplicateFields(data)
}

func loadLegacyAuthFile(path string) (*Account, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("auth file is not regular")
	}
	data, err := io.ReadAll(file)
	if err != nil || len(data) == 0 {
		return nil, errors.New("cannot read auth file")
	}
	var root struct {
		AuthMode string `json:"auth_mode"`
		Tokens   struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			IDToken      string `json:"id_token"`
			AccountID    string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	if root.AuthMode != "" && root.AuthMode != "chatgpt" {
		return nil, errors.New("unsupported auth mode")
	}
	if root.Tokens.AccessToken == "" && root.Tokens.RefreshToken == "" {
		return nil, errors.New("no tokens in auth file")
	}
	accountID := root.Tokens.AccountID
	var email, name, planType string
	for _, token := range []string{root.Tokens.IDToken, root.Tokens.AccessToken} {
		if token == "" {
			continue
		}
		claims := parseJWTClaims(token)
		if email == "" {
			email = claimsString(claims["email"])
		}
		if name == "" {
			name = claimsString(claims["name"])
		}
		authClaims := map[string]json.RawMessage{}
		_ = json.Unmarshal(claims["https://api.openai.com/auth"], &authClaims)
		if accountID == "" {
			accountID = claimsString(authClaims["chatgpt_account_id"])
		}
		if planType == "" {
			planType = claimsString(authClaims["chatgpt_plan_type"])
		}
	}

	acc := &Account{
		ID:           "default",
		Label:        "Default",
		Enabled:      true,
		AccessToken:  root.Tokens.AccessToken,
		RefreshToken: root.Tokens.RefreshToken,
		IDToken:      root.Tokens.IDToken,
		AccountID:    accountID,
		AccountEmail: email,
		AccountName:  name,
		PlanType:     planType,
	}
	return acc, nil
}

func parseJWTClaims(token string) map[string]json.RawMessage {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]json.RawMessage
	_ = json.Unmarshal(payload, &claims)
	return claims
}

func claimsString(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	return ""
}
