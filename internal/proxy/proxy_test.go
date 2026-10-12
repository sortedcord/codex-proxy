package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sortedcord/codex-proxy/internal/config"
	"github.com/sortedcord/codex-proxy/internal/oauth"
	"github.com/sortedcord/codex-proxy/internal/storage"
	"path/filepath"
)

const completedSSE = "data: {\"type\":\"response.completed\",\"response\":" +
	"{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"output\":[]}}\n\n"

func testProxy(t *testing.T, upstream *httptest.Server) *proxy {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	body := []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"test-access",` +
		`"refresh_token":"dummy-refresh","account_id":"test-account"}}`)
	if err := storage.AtomicWrite(path, body); err != nil {
		t.Fatal(err)
	}
	client := newHTTPClient()
	t.Cleanup(client.CloseIdleConnections)
	auth, err := oauth.NewManager(path, client, upstream.URL+"/oauth/token")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Addr: "127.0.0.1:8080", AuthFile: path, Upstream: upstream.URL,
		ClientVersion: config.DefaultClientVersion, UserAgent: "test-codex", IdleTimeout: time.Second,
	}
	return newProxy(
		cfg,
		auth,
		client,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
}

func requestProxy(p *proxy, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, request)
	return recorder
}

func TestResponseRoutesAndCredentialBoundary(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" || r.Method != http.MethodPost {
			t.Errorf("upstream route = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") == "Bearer local-secret" || r.Header.Get("Chatgpt-Account-Id") != "test-account" {
			t.Error("credential boundary violated")
		}
		if r.Header.Get("Cookie") != "" || r.Header.Get("X-Secret") != "" {
			t.Error("unsafe client headers forwarded")
		}
		if r.Header.Get("X-Codex-Turn-State") != "turn-state" || r.Header.Get("Session_id") != "session-1" {
			t.Error("session headers lost")
		}
		if r.Header.Get("X-Codex-Routing-Hint") != "model=gpt-test;tier=priority" {
			t.Error("routing hint not derived from payload")
		}
		object := map[string]json.RawMessage{}
		if err := json.NewDecoder(r.Body).Decode(&object); err != nil {
			t.Error(err)
		}
		if string(object["stream"]) != "true" || string(object["store"]) != "false" {
			t.Error("backend flags incorrect")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Codex-Turn-State", "next-state")
		w.Header().Set("Set-Cookie", "must-not-forward")
		_, _ = io.WriteString(w, completedSSE)
	}))
	defer upstream.Close()
	p := testProxy(t, upstream)
	p.cfg.APIKey = "local-secret"
	for _, path := range []string{"/v1/responses", "/backend-api/codex/responses", "/responses"} {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(
			`{"model":"gpt-test","input":"hello","service_tier":"fast"}`,
		))
		request.Header.Set("Authorization", "Bearer local-secret")
		request.Header.Set("Chatgpt-Account-Id", "malicious-account")
		request.Header.Set("Cookie", "client-secret")
		request.Header.Set("X-Secret", "client-secret")
		request.Header.Set("X-Codex-Turn-State", "turn-state")
		request.Header.Set("Session_id", "session-1")
		response := httptest.NewRecorder()
		p.ServeHTTP(response, request)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "resp_1") {
			t.Fatalf(
				"%s: %d %s",
				path,
				response.Code,
				response.Body.String(),
			)
		}
		if response.Header().Get("X-Codex-Turn-State") != "next-state" || response.Header().Get("Set-Cookie") != "" {
			t.Error("response header boundary violated")
		}
	}
}

func TestProxySurfaceAndValidation(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { calls.Add(1) }))
	defer upstream.Close()
	p := testProxy(t, upstream)
	cases := []struct {
		method string
		path   string
		body   string
		status int
	}{
		{method: "GET", path: "/health", status: 200},
		{method: "GET", path: "/healthz", status: 404},
		{method: "POST", path: "/v1/chat/completions", status: 404},
		{method: "POST", path: "/v1/messages", status: 404},
		{method: "POST", path: "/v1/models", status: 405},
		{method: "GET", path: "/v1/responses", status: 405},
		{method: "DELETE", path: "/v1/responses/resp_1", status: 404},
		{method: "POST", path: "/v1/responses", body: `null`, status: 400},
		{method: "POST", path: "/v1/responses", body: `{"model":"x","input":[],"background":true}`, status: 400},
	}
	for _, test := range cases {
		response := requestProxy(
			p,
			test.method,
			test.path,
			test.body,
		)
		if response.Code != test.status {
			t.Errorf(
				"%s %s: %d, want %d",
				test.method,
				test.path,
				response.Code,
				test.status,
			)
		}
	}
	response := requestProxy(
		p,
		"POST",
		"/v1/responses",
		strings.Repeat("x", maxRequestBytes+1),
	)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize request = %d", response.Code)
	}
	p.cfg.APIKey = "secret"
	if got := requestProxy(
		p,
		"POST",
		"/v1/responses",
		"{}",
	).Code; got != 401 {
		t.Errorf("missing key = %d", got)
	}
	if calls.Load() != 0 {
		t.Fatal("invalid requests reached upstream")
	}
}

func TestUpstreamErrorsAndCompactPassThrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/responses/compact" {
			object := map[string]json.RawMessage{}
			_ = json.NewDecoder(r.Body).Decode(&object)
			if _, ok := object["stream"]; ok {
				t.Error("compact stream field forwarded")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w,
				`{"object":"response.compaction","output":[`+
					`{"type":"compaction","encrypted_content":"opaque"}]}`,
			)
			return
		}
		w.Header().Set("Retry-After", "42")
		w.Header().Set("X-Request-Id", "request-1")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"usage_limit_reached","message":"quota"}}`)
	}))
	defer upstream.Close()
	p := testProxy(t, upstream)
	response := requestProxy(
		p,
		"POST",
		"/v1/responses",
		`{"model":"gpt-test","input":[]}`,
	)
	if response.Code != 429 || response.Header().Get("Retry-After") != "42" {
		t.Fatalf("quota response lost: %d %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "usage_limit_reached") {
		t.Fatal("upstream error lost")
	}
	response = requestProxy(
		p,
		"POST",
		"/v1/responses/compact",
		`{"model":"gpt-test","input":[]}`,
	)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "opaque") {
		t.Fatalf("compact response lost: %d %s", response.Code, response.Body.String())
	}
}

func TestModelCatalogFormats(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" || r.URL.Query().Get("client_version") == "" {
			t.Error("model request missing version")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Etag", `"catalog-1"`)
		if r.Header.Get("If-None-Match") != "" {
			w.WriteHeader(304)
			return
		}
		_, _ = io.WriteString(w, `{"models":[{"slug":"gpt-test","context_window":123,"supported_reasoning_levels":["low"]}]}`)
	}))
	defer upstream.Close()
	p := testProxy(t, upstream)
	response := requestProxy(
		p,
		"GET",
		"/v1/models",
		"",
	)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"id":"gpt-test"`) {
		t.Fatal("OpenAI model format missing")
	}
	response = requestProxy(
		p,
		"GET",
		"/backend-api/codex/models?client_version=0.162.1",
		"",
	)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "context_window") {
		t.Fatal("Codex model metadata lost")
	}
	request := httptest.NewRequest("GET", "/v1/models?client_version=0.162.1", nil)
	request.Header.Set("If-None-Match", `"catalog-1"`)
	response = httptest.NewRecorder()
	p.ServeHTTP(response, request)
	if response.Code != 304 || response.Body.Len() != 0 {
		t.Fatal("conditional model request broken")
	}
}

func Test401RecoveryIsBounded(t *testing.T) {
	for _, succeeds := range []bool{true, false} {
		t.Run(map[bool]string{true: "success", false: "rejected"}[succeeds], func(t *testing.T) {
			var inferenceCalls, refreshCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/oauth/token" {
					refreshCalls.Add(1)
					_, _ = io.WriteString(w, `{"access_token":"new-access","refresh_token":"new-refresh"}`)
					return
				}
				inferenceCalls.Add(1)
				if !succeeds || r.Header.Get("Authorization") != "Bearer new-access" {
					w.WriteHeader(401)
					_, _ = io.WriteString(w, `{"error":"unauthorized"}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, completedSSE)
			}))
			defer upstream.Close()
			p := testProxy(t, upstream)
			response := requestProxy(
				p,
				"POST",
				"/v1/responses",
				`{"model":"gpt-test","input":[]}`,
			)
			want := 401
			if succeeds {
				want = 200
			}
			expectedCalls := inferenceCalls.Load() == 2 && refreshCalls.Load() == 1
			if response.Code != want || !expectedCalls {
				t.Fatalf(
					"recovery: status=%d, inference=%d, refresh=%d",
					response.Code,
					inferenceCalls.Load(),
					refreshCalls.Load(),
				)
			}
		})
	}
}

func TestRedirectCannotLeakCredentials(t *testing.T) {
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { leaked.Store(true) }))
	defer target.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(
			w,
			r,
			target.URL,
			http.StatusTemporaryRedirect,
		)
	}))
	defer upstream.Close()
	p := testProxy(t, upstream)
	response := requestProxy(
		p,
		"GET",
		"/v1/models",
		"",
	)
	unsafeRedirect := leaked.Load() || response.Header().Get("Location") != ""
	if response.Code != 307 || unsafeRedirect {
		t.Fatal("credential-carrying redirect followed or exposed")
	}
}

func TestStreamingFlushAndDisconnectCancellation(t *testing.T) {
	canceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}))
	defer upstream.Close()
	p := testProxy(t, upstream)
	server := httptest.NewServer(p)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(
		ctx,
		"POST",
		server.URL+"/v1/responses",
		strings.NewReader(`{"model":"gpt-test","input":[],"stream":true}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Header.Get("Content-Type") != "text/event-stream" || resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Error("streaming headers missing")
	}
	buffer := make([]byte, 8)
	if _, err := io.ReadFull(resp.Body, buffer); err != nil {
		t.Fatal("first event was not flushed", err)
	}
	cancel()
	_ = resp.Body.Close()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream was not canceled on client disconnect")
	}
}

func TestUpstreamIdleTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	p := testProxy(t, upstream)
	p.cfg.IdleTimeout = 30 * time.Millisecond
	start := time.Now()
	response := requestProxy(
		p,
		"POST",
		"/v1/responses",
		`{"model":"gpt-test","input":[]}`,
	)
	if response.Code != 502 || time.Since(start) > time.Second {
		t.Fatal("idle upstream did not terminate promptly")
	}
}
