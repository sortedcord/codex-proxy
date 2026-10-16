package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sortedcord/codex-proxy/internal/apikey"
	"github.com/sortedcord/codex-proxy/internal/config"
)

func setupTestConfig(t *testing.T) (config.Config, string) {
	t.Helper()
	home := t.TempDir()
	configDir := filepath.Join(home, ".config", "codex-proxy")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDir, "config.json")

	cfg := config.Config{
		Addr: "127.0.0.1:8080",
		Accounts: []config.Account{
			{
				ID:           "work",
				Label:        "Work",
				Enabled:      true,
				AccountEmail: "work@example.com",
				Keys: []config.AccountKey{
					{
						ID:        "key_1",
						Label:     "bifrost",
						Hash:      apikey.Hash("cxp_test_secret"),
						Last4:     "cret",
						CreatedAt: time.Now().UTC(),
					},
				},
			},
		},
		HistoryDir:        filepath.Join(home, "history"),
		PollInterval:      5 * time.Minute,
		HistoryMaxSamples: 100,
		ClientVersion:     config.DefaultClientVersion,
		Upstream:          config.DefaultUpstream,
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg, configPath
}

func TestCLIAdminKeyRotate(t *testing.T) {
	_, path := setupTestConfig(t)
	var output bytes.Buffer

	if err := runAdminKeyCommand([]string{"rotate"}, path, &output); err != nil {
		t.Fatalf("runAdminKeyCommand error = %v", err)
	}

	outStr := output.String()
	if !strings.Contains(outStr, "Rotated admin API key:") || !strings.Contains(outStr, "cxp_") {
		t.Fatalf("unexpected admin-key rotate output: %s", outStr)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		AdminKeyHash string `json:"adminKeyHash"`
	}
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(saved.AdminKeyHash, "sha256:") {
		t.Fatalf("expected adminKeyHash to start with sha256:, got %q", saved.AdminKeyHash)
	}
}

func TestCLIAccountsCommands(t *testing.T) {
	cfg, path := setupTestConfig(t)
	var output bytes.Buffer

	// List
	if err := runAccountsCommand([]string{"list"}, cfg, path, &output); err != nil {
		t.Fatalf("accounts list error = %v", err)
	}
	if !strings.Contains(output.String(), "work") || !strings.Contains(output.String(), "enabled") {
		t.Fatalf("unexpected accounts list output: %s", output.String())
	}

	// Disable
	output.Reset()
	if err := runAccountsCommand([]string{"disable", "work"}, cfg, path, &output); err != nil {
		t.Fatalf("accounts disable error = %v", err)
	}
	if !strings.Contains(output.String(), `Account "work" disabled.`) {
		t.Fatalf("unexpected disable output: %s", output.String())
	}

	// Enable
	output.Reset()
	if err := runAccountsCommand([]string{"enable", "work"}, cfg, path, &output); err != nil {
		t.Fatalf("accounts enable error = %v", err)
	}
	if !strings.Contains(output.String(), `Account "work" enabled.`) {
		t.Fatalf("unexpected enable output: %s", output.String())
	}

	// Remove
	output.Reset()
	if err := runAccountsCommand([]string{"remove", "work"}, cfg, path, &output); err != nil {
		t.Fatalf("accounts remove error = %v", err)
	}
	if !strings.Contains(output.String(), `Account "work" removed.`) {
		t.Fatalf("unexpected remove output: %s", output.String())
	}
}

func TestCLIKeysCommands(t *testing.T) {
	cfg, path := setupTestConfig(t)
	var output bytes.Buffer

	// List
	if err := runKeysCommand([]string{"list", "work"}, cfg, path, &output); err != nil {
		t.Fatalf("keys list error = %v", err)
	}
	if !strings.Contains(output.String(), "key_1") || !strings.Contains(output.String(), "bifrost") {
		t.Fatalf("unexpected keys list output: %s", output.String())
	}

	// Create
	output.Reset()
	if err := runKeysCommand([]string{"create", "work", "--label", "dev-key"}, cfg, path, &output); err != nil {
		t.Fatalf("keys create error = %v", err)
	}
	outStr := output.String()
	if !strings.Contains(outStr, "Created API key for account \"work\":") || !strings.Contains(outStr, "cxp_") {
		t.Fatalf("unexpected keys create output: %s", outStr)
	}

	// Revoke
	output.Reset()
	if err := runKeysCommand([]string{"revoke", "work", "key_1"}, cfg, path, &output); err != nil {
		t.Fatalf("keys revoke error = %v", err)
	}
	if !strings.Contains(output.String(), `Key "key_1" revoked from account "work".`) {
		t.Fatalf("unexpected keys revoke output: %s", output.String())
	}
}
