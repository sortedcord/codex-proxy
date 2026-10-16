package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sortedcord/codex-proxy/internal/apikey"
)

func TestValidateAccountID(t *testing.T) {
	valid := []string{"default", "work", "personal", "acc-1", "a", "1", "account-123"}
	for _, id := range valid {
		if err := ValidateAccountID(id); err != nil {
			t.Errorf("ValidateAccountID(%q) = %v, want nil", id, err)
		}
	}

	invalid := []string{
		"",
		"-start",
		"end-",
		"UPPER",
		"has space",
		"has_underscore",
		"a-very-long-account-id-exceeding-thirty-two-chars",
	}
	for _, id := range invalid {
		if err := ValidateAccountID(id); err == nil {
			t.Errorf("ValidateAccountID(%q) = nil, want error", id)
		}
	}
}

func TestDeriveAccountID(t *testing.T) {
	cases := []struct {
		email    string
		existing []string
		want     string
	}{
		{"developer@example.com", nil, "developer"},
		{"DEV.ELOPER@example.com", nil, "dev-eloper"},
		{"first.last+tag@work.co", nil, "first-last-tag"},
		{"user@example.com", []string{"user"}, "user-2"},
		{"user@example.com", []string{"user", "user-2"}, "user-3"},
		{"@empty.com", nil, "account"},
		{"---@empty.com", nil, "account"},
	}
	for _, tc := range cases {
		got := DeriveAccountID(tc.email, tc.existing)
		if got != tc.want {
			t.Errorf("DeriveAccountID(%q, %v) = %q, want %q", tc.email, tc.existing, got, tc.want)
		}
		if err := ValidateAccountID(got); err != nil {
			t.Errorf("DeriveAccountID produced invalid slug %q: %v", got, err)
		}
	}
}

func TestLegacyConfigMigrationOnLoad(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")

	legacyJSON := `{
  "addr": "127.0.0.1:8080",
  "apiKey": "secret-api-key",
  "refreshToken": "legacy-refresh",
  "accessToken": "legacy-access",
  "accountId": "openai-12345",
  "accountEmail": "dev@example.com",
  "accountName": "Developer Name",
  "planType": "plus"
}`
	if err := os.WriteFile(configPath, []byte(legacyJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Parse([]string{"-config", configPath}, os.Stderr)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if len(cfg.Accounts) != 1 {
		t.Fatalf("len(cfg.Accounts) = %d, want 1", len(cfg.Accounts))
	}
	acc := cfg.Accounts[0]
	if acc.ID != "default" || acc.AccountEmail != "dev@example.com" || acc.AccountID != "openai-12345" {
		t.Fatalf("migrated account = %+v, want default dev@example.com", acc)
	}
	if len(acc.Keys) != 1 {
		t.Fatalf("len(acc.Keys) = %d, want 1", len(acc.Keys))
	}
	key := acc.Keys[0]
	if key.ID != "key_legacy" {
		t.Errorf("key ID = %q, want key_legacy", key.ID)
	}
	if !apikey.Match("secret-api-key", key.Hash) {
		t.Errorf("key hash did not match secret-api-key")
	}
}

func TestLegacyAuthFileMigrationOnLoad(t *testing.T) {
	tempDir := t.TempDir()
	authPath := filepath.Join(tempDir, "auth.json")
	configPath := filepath.Join(tempDir, "nonexistent-config.json")

	authJSON := `{
  "auth_mode": "chatgpt",
  "last_refresh": "2026-10-15T12:00:00Z",
  "tokens": {
    "access_token": "token-access",
    "refresh_token": "token-refresh",
    "id_token": "token-id",
    "account_id": "account-auto-id"
  }
}`
	if err := os.WriteFile(authPath, []byte(authJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Parse([]string{"-auth-file", authPath, "-config", configPath}, os.Stderr)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if len(cfg.Accounts) != 1 {
		t.Fatalf("len(cfg.Accounts) = %d, want 1", len(cfg.Accounts))
	}
	acc := cfg.Accounts[0]
	if acc.ID != "default" || acc.AccountID != "account-auto-id" {
		t.Fatalf("migrated account = %+v, want default account-auto-id", acc)
	}
	if acc.AccessToken != "token-access" || acc.RefreshToken != "token-refresh" {
		t.Fatalf("tokens not migrated properly: %+v", acc)
	}

	// Ensure auth.json is left untouched
	content, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != authJSON {
		t.Fatalf("auth.json was modified: got %s, want %s", string(content), authJSON)
	}
}

func TestUpdateAccountsPersistsAndStripsLegacyFields(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")

	legacyJSON := `{
  "addr": "127.0.0.1:8080",
  "apiKey": "old-key",
  "refreshToken": "old-refresh",
  "accessToken": "old-access",
  "accountId": "openai-12345",
  "accountEmail": "dev@example.com"
}`
	if err := os.WriteFile(configPath, []byte(legacyJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	rawSecret, _ := apikey.Generate()
	newKey := AccountKey{
		ID:        "key_custom",
		Label:     "custom",
		Hash:      apikey.Hash(rawSecret),
		Last4:     apikey.Last4(rawSecret),
		CreatedAt: time.Now().UTC(),
	}

	err := UpdateAccounts(configPath, func(accounts *[]Account) error {
		if len(*accounts) != 1 || (*accounts)[0].ID != "default" {
			t.Fatalf("expected 1 default migrated account, got %v", *accounts)
		}
		(*accounts)[0].Keys = append((*accounts)[0].Keys, newKey)
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateAccounts error = %v", err)
	}

	// Verify disk file
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	rawStr := string(raw)

	var topLevel map[string]json.RawMessage
	if err := json.Unmarshal(raw, &topLevel); err != nil {
		t.Fatalf("unmarshal updated config: %v", err)
	}

	for _, stripped := range []string{"apiKey", "refreshToken", "accessToken", "accountId", "accountEmail"} {
		if _, exists := topLevel[stripped]; exists {
			t.Errorf("top-level saved config still contains stripped field %q:\n%s", stripped, rawStr)
		}
	}

	var parsed struct {
		Accounts []Account `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal updated config: %v", err)
	}
	if len(parsed.Accounts) != 1 || len(parsed.Accounts[0].Keys) != 2 {
		t.Fatalf("expected 1 account with 2 keys, got %+v", parsed.Accounts)
	}
}

func TestValidateAccountsUniqueness(t *testing.T) {
	k1, _ := apikey.Generate()
	k2, _ := apikey.Generate()

	valid := []Account{
		{
			ID:        "acc-1",
			AccountID: "oa-1",
			Enabled:   true,
			Keys: []AccountKey{
				{ID: "k1", Hash: apikey.Hash(k1)},
			},
		},
		{
			ID:        "acc-2",
			AccountID: "oa-2",
			Enabled:   true,
			Keys: []AccountKey{
				{ID: "k2", Hash: apikey.Hash(k2)},
			},
		},
	}
	if err := ValidateAccounts(valid); err != nil {
		t.Fatalf("ValidateAccounts(valid) = %v", err)
	}

	// Duplicate account ID
	dupID := []Account{
		{ID: "same", Enabled: true},
		{ID: "same", Enabled: true},
	}
	if err := ValidateAccounts(dupID); err == nil {
		t.Errorf("ValidateAccounts expected error for duplicate account ID")
	}

	// Duplicate OpenAI ID
	dupOA := []Account{
		{ID: "acc-1", AccountID: "same-oa", Enabled: true},
		{ID: "acc-2", AccountID: "same-oa", Enabled: true},
	}
	if err := ValidateAccounts(dupOA); err == nil {
		t.Errorf("ValidateAccounts expected error for duplicate ChatGPT account ID")
	}

	// Duplicate key ID within account
	dupKeyID := []Account{
		{
			ID:      "acc-1",
			Enabled: true,
			Keys: []AccountKey{
				{ID: "k1", Hash: apikey.Hash(k1)},
				{ID: "k1", Hash: apikey.Hash(k2)},
			},
		},
	}
	if err := ValidateAccounts(dupKeyID); err == nil {
		t.Errorf("ValidateAccounts expected error for duplicate key ID within account")
	}

	// Duplicate key hash across accounts
	dupKeyHash := []Account{
		{
			ID:      "acc-1",
			Enabled: true,
			Keys: []AccountKey{
				{ID: "k1", Hash: apikey.Hash(k1)},
			},
		},
		{
			ID:      "acc-2",
			Enabled: true,
			Keys: []AccountKey{
				{ID: "k2", Hash: apikey.Hash(k1)},
			},
		},
	}
	if err := ValidateAccounts(dupKeyHash); err == nil {
		t.Errorf("ValidateAccounts expected error for duplicate key hash across accounts")
	}
}

func TestLoopbackMultipleAccountsRequireKeys(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")

	cfgJSON := `{
  "addr": "127.0.0.1:8080",
  "accounts": [
    {"id": "work", "enabled": true},
    {"id": "personal", "enabled": true}
  ]
}`
	if err := os.WriteFile(configPath, []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Parse([]string{"-config", configPath}, os.Stderr)
	if err == nil {
		t.Fatalf("expected error when multiple loopback accounts have zero keys")
	}
	if !strings.Contains(err.Error(), "API keys are required when multiple accounts are configured") {
		t.Fatalf("unexpected error: %v", err)
	}
}
