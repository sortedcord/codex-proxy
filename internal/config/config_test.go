package config

import (
	"io"
	"path/filepath"
	"testing"
	"time"
)

func TestConfigDefaultsAndEnvironment(t *testing.T) {
	home := t.TempDir()
	for _, name := range []string{
		"CODEX_PROXY_ADDR", "CODEX_PROXY_AUTH_FILE", "CODEX_PROXY_UPSTREAM",
		"CODEX_PROXY_API_KEY", "CODEX_PROXY_MANAGEMENT_KEY", "CODEX_PROXY_USAGE_URL",
		"CODEX_PROXY_QUOTA_POLL_INTERVAL", "CODEX_PROXY_HISTORY_DIR",
		"CODEX_PROXY_CLIENT_VERSION", "CODEX_PROXY_USER_AGENT",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("CODEX_HOME", home)
	cfg, err := Parse([]string{}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "auth.json"); cfg.AuthFile != want {
		t.Fatalf("default auth file=%q, want %q", cfg.AuthFile, want)
	}
	if cfg.Addr != "127.0.0.1:8080" {
		t.Fatalf("default address=%q, want 127.0.0.1:8080", cfg.Addr)
	}
	if _, err := Parse([]string{"-addr", "0.0.0.0:8080"}, io.Discard); err == nil {
		t.Fatal("unprotected external listener allowed")
	}
	t.Setenv("CODEX_PROXY_API_KEY", "local-secret")
	if _, err := Parse([]string{"-addr", "0.0.0.0:8080"}, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestConfigRejectsUnsafeURLsAndInvalidFlags(t *testing.T) {
	t.Setenv("CODEX_PROXY_API_KEY", "")
	for _, upstream := range []string{
		"http://example.com", "ftp://example.com", "https://user:pass@example.com",
		"https://example.com?secret=yes", "https://example.com#fragment", "relative/path",
	} {
		if _, err := Parse([]string{"-upstream", upstream}, io.Discard); err == nil {
			t.Errorf("unsafe upstream accepted: %q", upstream)
		}
	}
	for _, args := range [][]string{
		{"-addr", ":8080"}, {"-idle-timeout", "0s"}, {"-addr", "bad"}, {"unexpected"},
	} {
		if _, err := Parse(args, io.Discard); err == nil {
			t.Errorf("invalid flags accepted: %v", args)
		}
	}
}

func TestFlagsOverrideInvalidEnvironmentDefault(t *testing.T) {
	t.Setenv("CODEX_PROXY_QUOTA_POLL_INTERVAL", "invalid-duration")
	cfg, err := Parse([]string{"-quota-poll-interval", "1m"}, io.Discard)
	if err != nil {
		t.Fatalf("explicit valid flag did not override invalid environment default: %v", err)
	}
	if cfg.PollInterval != 1*time.Minute {
		t.Fatalf("poll interval = %v, want 1m", cfg.PollInterval)
	}
}

func TestExplicitPathsDoNotRequireHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("CODEX_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	dir := t.TempDir()
	authFile := filepath.Join(dir, "auth.json")
	historyDir := filepath.Join(dir, "history")
	cfg, err := Parse([]string{
		"-auth-file", authFile,
		"-history-dir", historyDir,
	}, io.Discard)
	if err != nil {
		t.Fatalf("explicit paths still failed when home is unresolvable: %v", err)
	}
	if cfg.AuthFile != authFile {
		t.Fatalf("auth file=%q, want %q", cfg.AuthFile, authFile)
	}
	if cfg.HistoryDir != historyDir {
		t.Fatalf("history directory=%q, want %q", cfg.HistoryDir, historyDir)
	}
}
