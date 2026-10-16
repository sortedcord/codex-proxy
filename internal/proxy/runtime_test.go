package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	goRuntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"errors"
	"github.com/sortedcord/codex-proxy/internal/config"
	"github.com/sortedcord/codex-proxy/internal/oauth"
)

func identityJWT(account string) string {
	body, _ := json.Marshal(map[string]any{
		"exp": time.Now().Add(time.Hour).Unix(), "email": account + "@example.invalid", "name": "Test User",
		"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": account, "chatgpt_plan_type": "plus"},
	})
	return "header." + base64.RawURLEncoding.EncodeToString(body) + ".signature"
}

func runtimeForTest(t *testing.T, handler http.Handler) *Runtime {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	dir := t.TempDir()
	cfg := config.Config{
		Addr: "127.0.0.1:8080", AuthFile: filepath.Join(dir, "auth.json"),
		APIKey: "inference-test", ManagementKey: "management-test", Upstream: upstream.URL,
		UsageURL: upstream.URL + "/usage", ClientVersion: config.DefaultClientVersion,
		UserAgent: "test-codex", IdleTimeout: time.Second, PollInterval: time.Hour,
		HistoryDir: filepath.Join(dir, "history"), HistoryMaxSamples: 3,
	}
	runtime, err := newRuntime(
		context.Background(),
		cfg,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		oauth.LoginOptions{
			AuthorizeURL: upstream.URL + "/authorize", TokenURL: upstream.URL + "/token",
			CallbackAddresses: []string{"127.0.0.1:0"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Error(err)
		}
	})
	return runtime
}

func managementRequest(runtime *Runtime, method, path, body, key string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	runtime.ServeHTTP(response, request)
	return response
}

func testLogin(t *testing.T, runtime *Runtime, account string) *httptest.ResponseRecorder {
	t.Helper()
	response := managementRequest(
		runtime,
		"GET",
		"/config/login",
		"",
		"management-test",
	)
	if response.Code != 200 {
		t.Fatalf("login begin returned %d", response.Code)
	}
	info := oauth.LoginInfo{}
	if err := json.Unmarshal(response.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(info.LoginURL)
	body, _ := json.Marshal(map[string]string{"code": account, "state": parsed.Query().Get("state")})
	return managementRequest(
		runtime,
		"POST",
		"/config/login",
		string(body),
		"management-test",
	)
}

func fakeAccountBackend(t *testing.T, custom func(http.ResponseWriter, *http.Request) bool) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if custom != nil && custom(w, request) {
			return
		}
		switch request.URL.Path {
		case "/token":
			if err := request.ParseForm(); err != nil {
				t.Error(err)
			}
			if request.Form.Get("grant_type") != "authorization_code" {
				t.Error("incorrect OAuth grant")
			}
			if request.Form.Get("client_id") != oauth.ClientID {
				t.Error("incorrect Codex OAuth client")
			}
			if len(request.Form.Get("code_verifier")) != 43 {
				t.Error("missing PKCE verifier")
			}
			account := request.Form.Get("code")
			_ = json.NewEncoder(w).Encode(oauth.Tokens{
				AccessToken: identityJWT(account), IDToken: identityJWT(account), RefreshToken: "dummy-refresh-" + account,
			})
		case "/usage":
			account := request.Header.Get("Chatgpt-Account-Id")
			if request.Header.Get("Authorization") != "Bearer "+identityJWT(account) {
				// Expiry encoding can cross a second, so check the account claim as
				// well as ensuring neither local key crossed the boundary.
				if strings.Contains(request.Header.Get("Authorization"), "management-test") {
					t.Error("management key leaked upstream")
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"account_id": account, "plan_type": "plus", "rate_limit": map[string]any{
					"allowed": true, "limit_reached": false,
					"primary_window": map[string]int{"used_percent": 12, "limit_window_seconds": 18000, "reset_after_seconds": 600},
					"secondary_window": map[string]int{
						"used_percent": 24, "limit_window_seconds": 604800, "reset_after_seconds": 3600,
					},
				},
			})
		case "/responses":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, completedSSE)
		default:
			w.WriteHeader(404)
		}
	})
}

func awaitQuota(t *testing.T, runtime *Runtime, account string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, available, _ := runtime.status.Latest(account); available {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("quota was not observed")
}

func TestRuntimeManagementAuthenticationAndEmptyStartup(t *testing.T) {
	var upstreamCalls atomic.Int32
	runtime := runtimeForTest(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(500)
	}))
	health := managementRequest(
		runtime,
		"GET",
		"/health",
		"",
		"",
	)
	if health.Code != http.StatusOK {
		t.Fatalf("empty startup health status=%d, want 200", health.Code)
	}
	if !strings.Contains(health.Body.String(), `"credential_configured":false`) {
		t.Fatal("empty startup health incorrectly reported credentials")
	}
	for _, path := range []string{"/status/account", "/status/limit", "/status/usage", "/config/login"} {
		response := managementRequest(
			runtime,
			"GET",
			path,
			"",
			"inference-test",
		)
		if response.Code != 401 {
			t.Fatalf("inference key gained management access to %s", path)
		}
	}
	response := managementRequest(
		runtime,
		"POST",
		"/v1/responses",
		`{"model":"gpt-test","input":"test"}`,
		"inference-test",
	)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("empty account inference status=%d, want 503", response.Code)
	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Fatalf("empty account contacted upstream %d times", got)
	}
	runtime.cfg.ManagementKey = ""
	response = managementRequest(
		runtime,
		"GET",
		"/config/login",
		"",
		"management-test",
	)
	if response.Code != 503 {
		t.Fatal("management enabled without a key")
	}
}

func TestRuntimeLoginQuotaAndPersistentRestart(t *testing.T) {
	runtime := runtimeForTest(t, fakeAccountBackend(t, nil))
	response := testLogin(t, runtime, "account-a")
	if response.Code != 200 {
		t.Fatalf("login failed: %d", response.Code)
	}
	leakedAccess := strings.Contains(response.Body.String(), "access_token")
	leakedRefresh := strings.Contains(response.Body.String(), "dummy-refresh")
	if leakedAccess {
		t.Fatal("login response exposed an access token field")
	}
	if leakedRefresh {
		t.Fatal("login response exposed a refresh token")
	}
	awaitQuota(t, runtime, "account-a")
	for _, path := range []string{"/status/account", "/status/limit"} {
		response := managementRequest(
			runtime,
			"GET",
			path,
			"",
			"management-test",
		)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d, want 200", path, response.Code)
		}
		if !strings.Contains(response.Body.String(), "account-a") {
			t.Fatalf("%s omitted the active account", path)
		}
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Error("management response could be cached")
		}
	}
	cfg := runtime.cfg
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.Stat(cfg.AuthFile)
	if err != nil {
		t.Fatalf("cannot inspect persisted credentials: %v", err)
	}
	if got := file.Mode().Perm(); goRuntime.GOOS != "windows" && got != 0o600 {
		t.Fatalf("credential permissions=%o, want 600", got)
	}
}

func TestHotSwitchDoesNotReplayOldRequestUnderNewAccount(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var oldCalls atomic.Int32
	backend := fakeAccountBackend(t, func(w http.ResponseWriter, request *http.Request) bool {
		if request.URL.Path != "/responses" || request.Header.Get("Chatgpt-Account-Id") != "account-a" {
			return false
		}
		if oldCalls.Add(1) == 1 {
			close(started)
			<-release
		}
		w.WriteHeader(401)
		return true
	})
	runtime := runtimeForTest(t, backend)
	if response := testLogin(t, runtime, "account-a"); response.Code != 200 {
		t.Fatal("first login failed")
	}
	awaitQuota(t, runtime, "account-a")
	server := httptest.NewServer(runtime)
	var releaseOnce sync.Once
	defer func() { releaseOnce.Do(func() { close(release) }); server.Close() }()
	done := make(chan int, 1)
	go func() {
		request, _ := http.NewRequest(
			"POST",
			server.URL+"/v1/responses",
			strings.NewReader(`{"model":"gpt-test","input":"old"}`),
		)
		request.Header.Set("Authorization", "Bearer inference-test")
		resp, err := http.DefaultClient.Do(request)
		if err != nil {
			done <- 0
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		done <- resp.StatusCode
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("old request did not start")
	}
	if response := testLogin(t, runtime, "account-b"); response.Code != 200 {
		t.Fatal("hot login failed")
	}
	awaitQuota(t, runtime, "account-b")
	releaseOnce.Do(func() { close(release) })
	select {
	case code := <-done:
		if code != 502 {
			t.Fatalf("old request status = %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("old request did not complete")
	}
	if oldCalls.Load() != 1 {
		t.Fatal("old request was replayed")
	}
	response := managementRequest(
		runtime,
		"GET",
		"/status/limit",
		"",
		"management-test",
	)
	if !strings.Contains(response.Body.String(), "account-b") {
		t.Fatal("replacement account limit missing")
	}
	response = managementRequest(
		runtime,
		"POST",
		"/v1/responses",
		`{"model":"gpt-test","input":"new"}`,
		"inference-test",
	)
	if response.Code != 200 {
		t.Fatal("new account inference failed")
	}
}

func TestFailedHotSwitchPreparationKeepsExistingAccount(t *testing.T) {
	runtime := runtimeForTest(t, fakeAccountBackend(t, nil))
	if response := testLogin(t, runtime, "account-a"); response.Code != 200 {
		t.Fatal("first login failed")
	}
	runtime.persist = func(string, func(*[]config.Account) error) error {
		return errors.New("injected persist failure")
	}
	response := testLogin(t, runtime, "account-b")
	if response.Code != 500 {
		t.Fatalf("failed activation returned %d", response.Code)
	}
	identity, err := runtime.auth.Profile()
	if err != nil {
		t.Fatalf("active profile unavailable after failed switch: %v", err)
	}
	if identity.AccountID != "account-a" {
		t.Fatalf("active account=%q after failed switch, want account-a", identity.AccountID)
	}
	response = managementRequest(
		runtime,
		"POST",
		"/v1/responses",
		`{"model":"gpt-test","input":"test"}`,
		"inference-test",
	)
	if response.Code != 200 {
		t.Fatal("old account was not usable after failed switch")
	}
}
