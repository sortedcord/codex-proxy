package config

import (
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestInjectedEnvironmentPrecedence(t *testing.T) {
	root := t.TempDir()
	codexHome := filepath.Join(root, "codex")
	dataHome := filepath.Join(root, "data")
	values := map[string]string{
		"CODEX_HOME": codexHome, "XDG_DATA_HOME": dataHome,
		"CODEX_PROXY_QUOTA_POLL_INTERVAL": "bad", "CODEX_PROXY_ADDR": "bad",
	}
	env := environment{
		lookup: func(name string) (string, bool) { value, ok := values[name]; return value, ok },
		home:   func() (string, error) { return "", errors.New("no home") },
	}
	cfg, err := parse([]string{"-addr", "127.0.0.1:8080", "-quota-poll-interval", "1m"}, io.Discard, env)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(codexHome, "auth.json"); cfg.AuthFile != want {
		t.Fatalf("auth file=%q, want %q", cfg.AuthFile, want)
	}
	if want := filepath.Join(dataHome, "codex-proxy"); cfg.HistoryDir != want {
		t.Fatalf("history directory=%q, want %q", cfg.HistoryDir, want)
	}
}

func TestNoHomeDoesNotCreateRelativeDefaults(t *testing.T) {
	codexHome := filepath.Join(t.TempDir(), "codex")
	env := environment{
		lookup: func(name string) (string, bool) {
			if name == "CODEX_HOME" {
				return codexHome, true
			}
			return "", false
		},
		home: func() (string, error) { return "", errors.New("no home") },
	}
	_, err := parse(nil, io.Discard, env)
	if err == nil {
		t.Fatal("missing data home produced a configuration without an error")
	}
	if !strings.Contains(err.Error(), "history directory") {
		t.Fatalf("missing data home error=%v, want history directory error", err)
	}
}

func TestEndpointErrorsDoNotExposeSecrets(t *testing.T) {
	err := ValidateEndpoint("https://user:secret@bad host/usage?token=secret")
	if err == nil {
		t.Fatal("malformed endpoint accepted")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatal("URL parse error exposed credentials")
	}
}
