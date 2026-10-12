package main

import (
	"io"
	"path/filepath"
	"testing"
)

func TestConfigDefaultsAndEnvironment(t *testing.T) {
	home := t.TempDir()
	for _, name := range []string{
		"CODEX_PROXY_ADDR", "CODEX_PROXY_AUTH_FILE", "CODEX_PROXY_UPSTREAM",
		"CODEX_PROXY_API_KEY", "CODEX_PROXY_CLIENT_VERSION", "CODEX_PROXY_USER_AGENT",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("CODEX_HOME", home)
	cfg, err := parseConfig([]string{}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.authFile != filepath.Join(home, "auth.json") || cfg.addr != "127.0.0.1:8080" {
		t.Fatal("unsafe or incorrect defaults")
	}
	if _, err := parseConfig([]string{"-addr", "0.0.0.0:8080"}, io.Discard); err == nil {
		t.Fatal("unprotected external listener allowed")
	}
	t.Setenv("CODEX_PROXY_API_KEY", "local-secret")
	if _, err := parseConfig([]string{"-addr", "0.0.0.0:8080"}, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestConfigRejectsUnsafeURLsAndInvalidFlags(t *testing.T) {
	t.Setenv("CODEX_PROXY_API_KEY", "")
	for _, upstream := range []string{
		"http://example.com", "ftp://example.com", "https://user:pass@example.com",
		"https://example.com?secret=yes", "https://example.com#fragment", "relative/path",
	} {
		if _, err := parseConfig([]string{"-upstream", upstream}, io.Discard); err == nil {
			t.Errorf("unsafe upstream accepted: %q", upstream)
		}
	}
	for _, args := range [][]string{
		{"-addr", ":8080"}, {"-idle-timeout", "0s"}, {"-addr", "bad"}, {"unexpected"},
	} {
		if _, err := parseConfig(args, io.Discard); err == nil {
			t.Errorf("invalid flags accepted: %v", args)
		}
	}
}
