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
