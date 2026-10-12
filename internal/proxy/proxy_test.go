package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sortedcord/codex-proxy/internal/config"
	"github.com/sortedcord/codex-proxy/internal/oauth"
	"github.com/sortedcord/codex-proxy/internal/storage"
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
		if r.URL.Path != "/responses" {
			t.Errorf("upstream path=%q, want /responses", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("upstream method=%q, want POST", r.Method)
		}
		if r.Header.Get("Authorization") == "Bearer local-secret" {
			t.Error("inference key leaked upstream")
		}
		if r.Header.Get("Chatgpt-Account-Id") != "test-account" {
			t.Error("client overrode the upstream account")
		}
		if r.Header.Get("Cookie") != "" {
			t.Error("client cookie forwarded upstream")
		}
		if r.Header.Get("X-Secret") != "" {
			t.Error("unallowlisted client header forwarded upstream")
		}
		if r.Header.Get("X-Codex-Turn-State") != "turn-state" {
			t.Error("turn-state header lost")
		}
		if r.Header.Get("Session_id") != "session-1" {
			t.Error("session affinity header lost")
		}
		if r.Header.Get("X-Codex-Routing-Hint") != "model=gpt-test;tier=priority" {
			t.Error("routing hint not derived from payload")
		}
		object := map[string]json.RawMessage{}
		if err := json.NewDecoder(r.Body).Decode(&object); err != nil {
			t.Error(err)
		}
		if string(object["stream"]) != "true" {
			t.Error("backend stream=true flag missing")
		}
		if string(object["store"]) != "false" {
			t.Error("backend store=false flag missing")
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
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d, want 200", path, response.Code)
		}
		if !strings.Contains(response.Body.String(), "resp_1") {
			t.Fatalf("%s response identity missing", path)
		}
		if response.Header().Get("X-Codex-Turn-State") != "next-state" {
			t.Error("upstream turn-state header lost")
		}
		if response.Header().Get("Set-Cookie") != "" {
			t.Error("upstream cookie leaked downstream")
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
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("quota status=%d, want 429", response.Code)
	}
	if got := response.Header().Get("Retry-After"); got != "42" {
		t.Fatalf("Retry-After=%q, want 42", got)
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
	if response.Code != http.StatusOK {
		t.Fatalf("compact status=%d, want 200", response.Code)
	}
	if !strings.Contains(response.Body.String(), "opaque") {
		t.Fatal("native compact output lost")
	}
}

func TestModelCatalogFormats(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("model discovery path=%q, want /models", r.URL.Path)
		}
		if r.URL.Query().Get("client_version") == "" {
			t.Error("model request missing client version")
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
	if response.Code != http.StatusOK {
		t.Fatalf("OpenAI model status=%d, want 200", response.Code)
	}
	if !strings.Contains(response.Body.String(), `"id":"gpt-test"`) {
		t.Fatal("OpenAI model ID missing")
	}
	response = requestProxy(
		p,
		"GET",
		"/backend-api/codex/models?client_version=0.162.1",
		"",
	)
	if response.Code != http.StatusOK {
		t.Fatalf("native catalog status=%d, want 200", response.Code)
	}
	if !strings.Contains(response.Body.String(), "context_window") {
		t.Fatal("Codex model metadata lost")
	}
	request := httptest.NewRequest("GET", "/v1/models?client_version=0.162.1", nil)
	request.Header.Set("If-None-Match", `"catalog-1"`)
	response = httptest.NewRecorder()
	p.ServeHTTP(response, request)
	if response.Code != http.StatusNotModified {
		t.Fatalf("conditional model status=%d, want 304", response.Code)
	}
	if response.Body.Len() != 0 {
		t.Fatal("304 model response contained a body")
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
			if response.Code != want {
				t.Fatalf("recovery status=%d, want %d", response.Code, want)
			}
			if got := inferenceCalls.Load(); got != 2 {
				t.Fatalf("inference attempts=%d, want 2", got)
			}
			if got := refreshCalls.Load(); got != 1 {
				t.Fatalf("refresh attempts=%d, want 1", got)
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
	if response.Code != http.StatusTemporaryRedirect {
		t.Fatalf("redirect status=%d, want 307", response.Code)
	}
	if leaked.Load() {
		t.Fatal("credential-carrying redirect followed")
	}
	if response.Header().Get("Location") != "" {
		t.Fatal("upstream redirect location exposed downstream")
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
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("stream content type=%q, want text/event-stream", got)
	}
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("stream buffering header=%q, want no", got)
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
	if response.Code != http.StatusBadGateway {
		t.Fatalf("idle timeout status=%d, want 502", response.Code)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("idle upstream termination took %s, want at most 1s", elapsed)
	}
}
