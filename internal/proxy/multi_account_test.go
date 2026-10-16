package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sortedcord/codex-proxy/internal/apikey"
	"github.com/sortedcord/codex-proxy/internal/config"
	"github.com/sortedcord/codex-proxy/internal/oauth"
)

func TestMultiAccountRoutingAndUpstreamBoundary(t *testing.T) {
	var workCalls atomic.Int32
	var personalCalls atomic.Int32

	workToken := newAdminJWT("oa-work")
	personalToken := newAdminJWT("oa-personal")

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/responses":
			accountID := r.Header.Get("Chatgpt-Account-Id")
			auth := r.Header.Get("Authorization")

			switch accountID {
			case "oa-work":
				workCalls.Add(1)
				if auth != "Bearer "+workToken {
					t.Errorf("expected work token %q in upstream header, got %q", workToken, auth)
				}
			case "oa-personal":
				personalCalls.Add(1)
				if auth != "Bearer "+personalToken {
					t.Errorf("expected personal token %q in upstream header, got %q", personalToken, auth)
				}
			default:
				t.Errorf("unexpected account ID in upstream header: %q", accountID)
			}

			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, completedSSE)
		default:
			w.WriteHeader(404)
		}
	}))
	defer backend.Close()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")

	workKey := "cxp_work_secret_12345678901234567890"
	personalKey := "cxp_personal_secret_12345678901234567890"

	cfg := config.Config{
		Addr: "127.0.0.1:8080",
		Accounts: []config.Account{
			{
				ID:           "work",
				Label:        "Work",
				Enabled:      true,
				AccessToken:  workToken,
				RefreshToken: "refresh-work",
				AccountID:    "oa-work",
				Keys: []config.AccountKey{
					{
						ID:        "k_work",
						Hash:      apikey.Hash(workKey),
						Last4:     apikey.Last4(workKey),
						CreatedAt: time.Now().UTC(),
					},
				},
			},
			{
				ID:           "personal",
				Label:        "Personal",
				Enabled:      true,
				AccessToken:  personalToken,
				RefreshToken: "refresh-personal",
				AccountID:    "oa-personal",
				Keys: []config.AccountKey{
					{
						ID:        "k_personal",
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
		Upstream:          backend.URL,
		UsageURL:          backend.URL + "/usage",
		ClientVersion:     config.DefaultClientVersion,
		UserAgent:         "test",
		IdleTimeout:       time.Minute,
	}

	cfgBytes, _ := json.MarshalIndent(cfg, "", "  ")
	_ = os.WriteFile(configPath, cfgBytes, 0o600)
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
	defer rt.Close()

	// 1. Request with workKey routes to work account
	req := httptest.NewRequest("POST", "/responses", strings.NewReader(`{"model":"gpt-test","input":"hello"}`))
	req.Header.Set("Authorization", "Bearer "+workKey)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("work request failed with %d: %s", w.Code, w.Body.String())
	}
	if workCalls.Load() != 1 {
		t.Fatalf("workCalls = %d, want 1", workCalls.Load())
	}

	// 2. Request with personalKey routes to personal account
	req = httptest.NewRequest("POST", "/responses", strings.NewReader(`{"model":"gpt-test","input":"hello"}`))
	req.Header.Set("Authorization", "Bearer "+personalKey)
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	rt.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("personal request failed with %d: %s", w.Code, w.Body.String())
	}
	if personalCalls.Load() != 1 {
		t.Fatalf("personalCalls = %d, want 1", personalCalls.Load())
	}

	// 3. Multi-account with no key presented returns 401
	req = httptest.NewRequest("POST", "/responses", strings.NewReader(`{"model":"gpt-test","input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	rt.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("multi-account request without key returned %d, want 401", w.Code)
	}

	// 4. Request with unknown key returns 401
	req = httptest.NewRequest("POST", "/responses", strings.NewReader(`{"model":"gpt-test","input":"hello"}`))
	req.Header.Set("Authorization", "Bearer cxp_unknown_key_999999999999")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	rt.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unknown key returned %d, want 401", w.Code)
	}
}

func TestLocalhostSingleAccountNoKeyConvenience(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, completedSSE)
	}))
	defer backend.Close()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")

	cfg := config.Config{
		Addr: "127.0.0.1:8080",
		Accounts: []config.Account{
			{
				ID:           "default",
				Label:        "Default",
				Enabled:      true,
				AccessToken:  newAdminJWT("oa-default"),
				RefreshToken: "refresh-default",
				AccountID:    "oa-default",
				Keys:         nil, // 0 keys configured
			},
		},
		HistoryDir:        filepath.Join(dir, "history"),
		HistoryMaxSamples: 10,
		PollInterval:      time.Hour,
		Upstream:          backend.URL,
		UsageURL:          backend.URL + "/usage",
		ClientVersion:     config.DefaultClientVersion,
		UserAgent:         "test",
		IdleTimeout:       time.Minute,
	}

	cfgBytes, _ := json.MarshalIndent(cfg, "", "  ")
	_ = os.WriteFile(configPath, cfgBytes, 0o600)
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
	defer rt.Close()

	// On loopback with 1 account and 0 keys, request without key must succeed
	req := httptest.NewRequest("POST", "/responses", strings.NewReader(`{"model":"gpt-test","input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("loopback single-account no-key request returned %d, want 200: %s", w.Code, w.Body.String())
	}
}

func TestMultiAccountLifecycleAndDynamicHotReload(t *testing.T) {
	var alphaCalls atomic.Int32
	var betaCalls atomic.Int32

	alphaToken := newAdminJWT("oa-alpha")
	betaToken := newAdminJWT("oa-beta")

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/responses":
			accountID := r.Header.Get("Chatgpt-Account-Id")
			auth := r.Header.Get("Authorization")

			switch accountID {
			case "oa-alpha":
				alphaCalls.Add(1)
				if auth != "Bearer "+alphaToken {
					t.Errorf("expected alpha token %q, got %q", alphaToken, auth)
				}
			case "oa-beta":
				betaCalls.Add(1)
				if auth != "Bearer "+betaToken {
					t.Errorf("expected beta token %q, got %q", betaToken, auth)
				}
			default:
				t.Errorf("unexpected account ID in upstream header: %q", accountID)
			}

			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, completedSSE)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer backend.Close()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")

	adminKey := "cxp_admin_secret_key_123456789012345678"
	alphaKey := "cxp_alpha_key_12345678901234567890"
	betaKey := "cxp_beta_key_12345678901234567890"

	cfg := config.Config{
		Addr:         "127.0.0.1:8080",
		AdminAPIKey:  adminKey,
		AdminKeyHash: apikey.Hash(adminKey),
		Accounts: []config.Account{
			{
				ID:           "alpha",
				Label:        "Alpha Account",
				Enabled:      true,
				AccessToken:  alphaToken,
				RefreshToken: "refresh-alpha",
				AccountID:    "oa-alpha",
				AccountEmail: "alpha@example.com",
				Keys: []config.AccountKey{
					{
						ID:        "k_alpha",
						Label:     "alpha-primary",
						Hash:      apikey.Hash(alphaKey),
						Last4:     apikey.Last4(alphaKey),
						CreatedAt: time.Now().UTC(),
					},
				},
			},
			{
				ID:           "beta",
				Label:        "Beta Account",
				Enabled:      true,
				AccessToken:  betaToken,
				RefreshToken: "refresh-beta",
				AccountID:    "oa-beta",
				AccountEmail: "beta@example.com",
				Keys: []config.AccountKey{
					{
						ID:        "k_beta",
						Label:     "beta-primary",
						Hash:      apikey.Hash(betaKey),
						Last4:     apikey.Last4(betaKey),
						CreatedAt: time.Now().UTC(),
					},
				},
			},
		},
		HistoryDir:        filepath.Join(dir, "history"),
		HistoryMaxSamples: 10,
		PollInterval:      time.Hour,
		Upstream:          backend.URL,
		UsageURL:          backend.URL + "/usage",
		ClientVersion:     config.DefaultClientVersion,
		UserAgent:         "test",
		IdleTimeout:       time.Minute,
	}

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
	defer rt.Close()

	// Spin up a real HTTP server over TCP loopback
	server := httptest.NewServer(rt)
	defer server.Close()
	client := server.Client()

	sendResponseReq := func(key string) (*http.Response, string) {
		req, err := http.NewRequest("POST", server.URL+"/responses", strings.NewReader(`{"model":"gpt-test","input":"test"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp, string(body)
	}

	// 1. Alpha request succeeds and routes to alpha
	resp, body := sendResponseReq(alphaKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for alpha, got %d: %s", resp.StatusCode, body)
	}
	if alphaCalls.Load() != 1 {
		t.Fatalf("expected 1 call to alpha upstream, got %d", alphaCalls.Load())
	}

	// 2. Beta request succeeds and routes to beta
	resp, body = sendResponseReq(betaKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for beta, got %d: %s", resp.StatusCode, body)
	}
	if betaCalls.Load() != 1 {
		t.Fatalf("expected 1 call to beta upstream, got %d", betaCalls.Load())
	}

	// 3. Verify key last used is recorded in Admin API
	adminReq, _ := http.NewRequest("GET", server.URL+"/admin/accounts/alpha/keys", nil)
	adminReq.Header.Set("Authorization", "Bearer "+adminKey)
	adminResp, err := client.Do(adminReq)
	if err != nil {
		t.Fatal(err)
	}
	var keysList []map[string]any
	_ = json.NewDecoder(adminResp.Body).Decode(&keysList)
	_ = adminResp.Body.Close()
	if len(keysList) != 1 || keysList[0]["last_used_at"] == nil {
		t.Fatalf("expected last_used_at to be recorded for alpha key: %+v", keysList)
	}

	// 4. Disable alpha via Admin API
	patchReq, _ := http.NewRequest("PATCH", server.URL+"/admin/accounts/alpha", strings.NewReader(`{"enabled":false}`))
	patchReq.Header.Set("Authorization", "Bearer "+adminKey)
	patchReq.Header.Set("Content-Type", "application/json")
	patchResp, err := client.Do(patchReq)
	if err != nil || patchResp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH disable alpha failed: code=%d err=%v", patchResp.StatusCode, err)
	}
	_ = patchResp.Body.Close()

	// 5. Alpha request now returns 403 Forbidden with account_disabled
	resp, body = sendResponseReq(alphaKey)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for disabled alpha account, got %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "account_disabled") {
		t.Fatalf("expected account_disabled in body, got: %s", body)
	}

	// 6. Beta request STILL succeeds while alpha is disabled
	resp, body = sendResponseReq(betaKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for beta while alpha disabled, got %d: %s", resp.StatusCode, body)
	}
	if betaCalls.Load() != 2 {
		t.Fatalf("expected 2 calls to beta upstream, got %d", betaCalls.Load())
	}

	// 7. Dynamically mint a new key for beta via Admin API
	createKeyReq, _ := http.NewRequest("POST", server.URL+"/admin/accounts/beta/keys", strings.NewReader(`{"label":"beta-second"}`))
	createKeyReq.Header.Set("Authorization", "Bearer "+adminKey)
	createKeyReq.Header.Set("Content-Type", "application/json")
	createKeyResp, err := client.Do(createKeyReq)
	if err != nil || createKeyResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST create key failed: code=%d err=%v", createKeyResp.StatusCode, err)
	}
	var createdKey struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	_ = json.NewDecoder(createKeyResp.Body).Decode(&createdKey)
	_ = createKeyResp.Body.Close()

	// 8. Request with newly minted key works IMMEDIATELY without restart
	resp, body = sendResponseReq(createdKey.Key)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 with newly minted key, got %d: %s", resp.StatusCode, body)
	}
	if betaCalls.Load() != 3 {
		t.Fatalf("expected 3 calls to beta upstream, got %d", betaCalls.Load())
	}

	// 9. Revoke the newly created key via Admin API
	deleteKeyReq, _ := http.NewRequest("DELETE", server.URL+"/admin/accounts/beta/keys/"+createdKey.ID, nil)
	deleteKeyReq.Header.Set("Authorization", "Bearer "+adminKey)
	deleteKeyResp, err := client.Do(deleteKeyReq)
	if err != nil || deleteKeyResp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE key failed: code=%d err=%v", deleteKeyResp.StatusCode, err)
	}
	_ = deleteKeyResp.Body.Close()

	// 10. Revoked key immediately fails with 401
	resp, body = sendResponseReq(createdKey.Key)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for revoked key, got %d: %s", resp.StatusCode, body)
	}

	// 11. Re-enable alpha via Admin API
	reEnableReq, _ := http.NewRequest("PATCH", server.URL+"/admin/accounts/alpha", strings.NewReader(`{"enabled":true}`))
	reEnableReq.Header.Set("Authorization", "Bearer "+adminKey)
	reEnableReq.Header.Set("Content-Type", "application/json")
	reEnableResp, err := client.Do(reEnableReq)
	if err != nil || reEnableResp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH re-enable alpha failed: code=%d err=%v", reEnableResp.StatusCode, err)
	}
	_ = reEnableResp.Body.Close()

	// 12. Alpha request succeeds again
	resp, body = sendResponseReq(alphaKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for re-enabled alpha, got %d: %s", resp.StatusCode, body)
	}
	if alphaCalls.Load() != 2 {
		t.Fatalf("expected 2 calls to alpha upstream, got %d", alphaCalls.Load())
	}
}
