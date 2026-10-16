package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sortedcord/codex-proxy/internal/apikey"
	"github.com/sortedcord/codex-proxy/internal/config"
	"github.com/sortedcord/codex-proxy/internal/oauth"
)

func newAdminJWT(account string) string {
	body, _ := json.Marshal(map[string]any{
		"exp": time.Now().Add(time.Hour).Unix(), "email": account + "@example.invalid", "name": "Test User",
		"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": account, "chatgpt_plan_type": "plus"},
	})
	return "header." + base64.RawURLEncoding.EncodeToString(body) + ".signature"
}

func newMultiAccountTestRuntime(t *testing.T) (*Runtime, string, string, string) {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")

	adminKey := "cxp_admin_secret_key_1234567890"
	workKey := "cxp_work_secret_key_1234567890"
	personalKey := "cxp_personal_secret_key_1234567890"

	cfg := config.Config{
		Addr:         "127.0.0.1:8080",
		AdminAPIKey:  adminKey,
		AdminKeyHash: apikey.Hash(adminKey),
		Accounts: []config.Account{
			{
				ID:           "work",
				Label:        "Work Account",
				Enabled:      true,
				AccessToken:  newAdminJWT("oa-work"),
				RefreshToken: "refresh-work",
				AccountID:    "oa-work",
				AccountEmail: "work@example.com",
				AccountName:  "Work User",
				PlanType:     "plus",
				Keys: []config.AccountKey{
					{
						ID:        "key_work_1",
						Label:     "work-key",
						Hash:      apikey.Hash(workKey),
						Last4:     apikey.Last4(workKey),
						CreatedAt: time.Now().UTC(),
					},
				},
			},
			{
				ID:           "personal",
				Label:        "Personal Account",
				Enabled:      true,
				AccessToken:  newAdminJWT("oa-personal"),
				RefreshToken: "refresh-personal",
				AccountID:    "oa-personal",
				AccountEmail: "personal@example.com",
				AccountName:  "Personal User",
				PlanType:     "pro",
				Keys: []config.AccountKey{
					{
						ID:        "key_personal_1",
						Label:     "personal-key",
						Hash:      apikey.Hash(personalKey),
						Last4:     apikey.Last4(personalKey),
						CreatedAt: time.Now().UTC(),
					},
				},
			},
		},
		HistoryDir:        filepath.Join(dir, "history"),
		HistoryMaxSamples: 10,
		PollInterval:      time.Hour,
		Upstream:          "https://chatgpt.com/backend-api/codex",
		UsageURL:          "https://chatgpt.com/backend-api/wham/usage",
		ClientVersion:     config.DefaultClientVersion,
		UserAgent:         "test-client",
		IdleTimeout:       time.Minute,
	}

	// Persist initial config.json
	cfgBytes, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, cfgBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.ConfigPath = configPath

	rt, err := newRuntime(
		context.Background(),
		cfg,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		oauth.LoginOptions{},
	)
	if err != nil {
		t.Fatalf("newRuntime failed: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	return rt, adminKey, workKey, personalKey
}

func TestAdminAuthentication(t *testing.T) {
	rt, adminKey, _, _ := newMultiAccountTestRuntime(t)

	// 1. Missing key returns 401
	req := httptest.NewRequest("GET", "/admin/accounts", nil)
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for missing key, got %d", w.Code)
	}

	// 2. Invalid key returns 401
	req = httptest.NewRequest("GET", "/admin/accounts", nil)
	req.Header.Set("Authorization", "Bearer invalid-admin-key")
	w = httptest.NewRecorder()
	rt.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for invalid key, got %d", w.Code)
	}

	// 3. Query parameter ?key= rejected on admin routes (allowQuery=false)
	req = httptest.NewRequest("GET", "/admin/accounts?key="+adminKey, nil)
	w = httptest.NewRecorder()
	rt.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for query param on admin route, got %d", w.Code)
	}

	// 4. Valid Authorization Bearer header
	req = httptest.NewRequest("GET", "/admin/accounts", nil)
	req.Header.Set("Authorization", "Bearer "+adminKey)
	w = httptest.NewRecorder()
	rt.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with valid Bearer key, got %d", w.Code)
	}

	// 5. Valid x-api-key header
	req = httptest.NewRequest("GET", "/admin/accounts", nil)
	req.Header.Set("x-api-key", adminKey)
	w = httptest.NewRecorder()
	rt.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with valid x-api-key, got %d", w.Code)
	}

	// 6. When no admin key is configured on server, returns 503
	rtNoAdmin, _, _, _ := newMultiAccountTestRuntime(t)
	rtNoAdmin.adminKeyHash = ""
	rtNoAdmin.adminAPIKey = ""
	req = httptest.NewRequest("GET", "/admin/accounts", nil)
	req.Header.Set("Authorization", "Bearer any-key")
	w = httptest.NewRecorder()
	rtNoAdmin.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when admin key unconfigured, got %d", w.Code)
	}
}

func TestAdminAccountsCRUD(t *testing.T) {
	rt, adminKey, _, _ := newMultiAccountTestRuntime(t)

	// List accounts
	req := httptest.NewRequest("GET", "/admin/accounts", nil)
	req.Header.Set("Authorization", "Bearer "+adminKey)
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /admin/accounts returned %d", w.Code)
	}
	var list []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(list))
	}

	// Get account details
	req = httptest.NewRequest("GET", "/admin/accounts/work", nil)
	req.Header.Set("Authorization", "Bearer "+adminKey)
	w = httptest.NewRecorder()
	rt.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /admin/accounts/work returned %d", w.Code)
	}
	var details map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &details); err != nil {
		t.Fatal(err)
	}
	if details["id"] != "work" || details["email"] != "oa-work@example.invalid" {
		t.Fatalf("unexpected account details: %+v", details)
	}

	// Patch account: disable work
	disableBody := `{"enabled":false,"label":"Work (Disabled)"}`
	req = httptest.NewRequest("PATCH", "/admin/accounts/work", strings.NewReader(disableBody))
	req.Header.Set("Authorization", "Bearer "+adminKey)
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	rt.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH /admin/accounts/work returned %d: %s", w.Code, w.Body.String())
	}

	// Verify disabled account returns 403 Forbidden for inference
	req = httptest.NewRequest("GET", "/models", nil)
	req.Header.Set("Authorization", "Bearer cxp_work_secret_key_1234567890")
	w = httptest.NewRecorder()
	rt.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for disabled account, got %d", w.Code)
	}

	// Re-enable work account
	enableBody := `{"enabled":true}`
	req = httptest.NewRequest("PATCH", "/admin/accounts/work", strings.NewReader(enableBody))
	req.Header.Set("Authorization", "Bearer "+adminKey)
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	rt.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH re-enable returned %d: %s", w.Code, w.Body.String())
	}

	// Delete account: delete personal
	req = httptest.NewRequest("DELETE", "/admin/accounts/personal", nil)
	req.Header.Set("Authorization", "Bearer "+adminKey)
	w = httptest.NewRecorder()
	rt.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE /admin/accounts/personal returned %d", w.Code)
	}

	// Verify personal account is gone
	req = httptest.NewRequest("GET", "/admin/accounts/personal", nil)
	req.Header.Set("Authorization", "Bearer "+adminKey)
	w = httptest.NewRecorder()
	rt.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("GET deleted account returned %d, want 404", w.Code)
	}
}

func TestAdminKeyManagement(t *testing.T) {
	rt, adminKey, _, _ := newMultiAccountTestRuntime(t)

	// List keys for work
	req := httptest.NewRequest("GET", "/admin/accounts/work/keys", nil)
	req.Header.Set("Authorization", "Bearer "+adminKey)
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /admin/accounts/work/keys returned %d", w.Code)
	}
	var keys []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &keys); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(keys))
	}

	// Create new key for work
	createBody := `{"label":"ci-key"}`
	req = httptest.NewRequest("POST", "/admin/accounts/work/keys", strings.NewReader(createBody))
	req.Header.Set("Authorization", "Bearer "+adminKey)
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	rt.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /admin/accounts/work/keys returned %d: %s", w.Code, w.Body.String())
	}
	var createdKey struct {
		ID    string `json:"id"`
		Label string `json:"label"`
		Key   string `json:"key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &createdKey); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(createdKey.Key, apikey.Prefix) {
		t.Fatalf("expected new key to start with %q, got %q", apikey.Prefix, createdKey.Key)
	}

	// Use newly created key to authenticate request
	req = httptest.NewRequest("GET", "/models", nil)
	req.Header.Set("Authorization", "Bearer "+createdKey.Key)
	w = httptest.NewRecorder()
	rt.ServeHTTP(w, req)
	// Even without upstream responses, it should pass auth and not return 401
	if w.Code == http.StatusUnauthorized {
		t.Fatalf("newly created key failed authentication with 401")
	}

	// Revoke the newly created key
	req = httptest.NewRequest("DELETE", "/admin/accounts/work/keys/"+createdKey.ID, nil)
	req.Header.Set("Authorization", "Bearer "+adminKey)
	w = httptest.NewRecorder()
	rt.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE key returned %d", w.Code)
	}

	// Revoked key now returns 401
	req = httptest.NewRequest("GET", "/models", nil)
	req.Header.Set("Authorization", "Bearer "+createdKey.Key)
	w = httptest.NewRecorder()
	rt.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for revoked key, got %d", w.Code)
	}
}

func TestAdminQuotaOverview(t *testing.T) {
	rt, adminKey, _, _ := newMultiAccountTestRuntime(t)

	req := httptest.NewRequest("GET", "/admin/quota", nil)
	req.Header.Set("Authorization", "Bearer "+adminKey)
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /admin/quota returned %d", w.Code)
	}
	var overview struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &overview); err != nil {
		t.Fatal(err)
	}
	if len(overview.Accounts) != 2 {
		t.Fatalf("expected 2 accounts in quota overview, got %d", len(overview.Accounts))
	}
}
