package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMissingUpstreamContentType(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Suppress Go's automatic Content-Type sniffing to reproduce the live gateway.
		w.Header()["Content-Type"] = []string{}
		_, _ = io.WriteString(w, completedSSE)
	}))
	defer upstream.Close()
	p := testProxy(t, upstream)
	for _, stream := range []bool{false, true} {
		body, _ := json.Marshal(map[string]any{"model": "gpt-test", "input": "test", "stream": stream})
		response := requestProxy(
			p,
			"POST",
			"/v1/responses",
			string(body),
		)
		if response.Code != 200 {
			t.Fatalf("missing Content-Type rejected: %d", response.Code)
		}
		if stream && response.Body.String() != completedSSE {
			t.Fatal("SSE passthrough changed bytes")
		}
	}
}

func TestCollectedInBandErrorsPreserveDetails(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			"data: {\"type\":\"error\",\"error\":{\"code\":\"usage_limit_reached\",\"message\":\"quota\"}}\n\n",
		)
	}))
	defer upstream.Close()
	response := requestProxy(
		testProxy(t, upstream),
		"POST",
		"/v1/responses",
		`{"model":"gpt-test","input":[]}`,
	)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("collected quota error status=%d, want 429", response.Code)
	}
	if !strings.Contains(response.Body.String(), "usage_limit_reached") {
		t.Fatal("collected quota error code lost")
	}
	for _, code := range []string{"server_is_overloaded", "unexpected_error"} {
		event := []byte(`{"type":"error","code":"` + code + `","message":"details"}`)
		err := newStreamError(event)
		if !json.Valid(err.body) {
			t.Fatal("invalid collected error JSON")
		}
		if strings.Contains(err.Error(), "details") {
			t.Fatal("error log message exposed upstream details")
		}
		if code == "server_is_overloaded" && err.status != 503 {
			t.Fatal("overload did not map to 503")
		}
	}
}

func TestConnectionNominatedHeadersAreNotForwarded(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Codex-Turn-State") != "" {
			t.Error("hop-by-hop request header forwarded")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Connection", "X-Codex-Turn-State")
		w.Header().Set("X-Codex-Turn-State", "hop-by-hop")
		_, _ = io.WriteString(w, completedSSE)
	}))
	defer upstream.Close()
	p := testProxy(t, upstream)
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":[]}`))
	req.Header.Set("Connection", "X-Codex-Turn-State")
	req.Header.Set("X-Codex-Turn-State", "hop-by-hop")
	response := httptest.NewRecorder()
	p.ServeHTTP(response, req)
	if response.Header().Get("X-Codex-Turn-State") != "" {
		t.Fatal("hop-by-hop response header forwarded")
	}
}

func TestCacheBreakpointsAndInvalidArguments(t *testing.T) {
	prepared, err := prepareRequest([]byte(`{
		"model":"gpt-test","input":[
			{"role":"user","prompt_cache_breakpoint":true,"content":[
				{"type":"input_text","text":"hello","prompt_cache_breakpoint":true}
			]},
			{"type":"function_call","name":"invalid","arguments":{"leave":"unchanged"}},
			{"type":"function_call","name":"blank","arguments":"  "},
			{"type":"function_call_output","call_id":"c","output":[
				{"type":"input_text","text":"result","prompt_cache_breakpoint":true}
			]}
		]
	}`), false)
	if err != nil {
		t.Fatal(err)
	}
	body := string(prepared.body)
	if strings.Contains(body, "prompt_cache_breakpoint") {
		t.Fatal("cache hints were retained")
	}
	if !strings.Contains(body, `"leave":"unchanged"`) {
		t.Fatal("non-string arguments were silently rewritten")
	}
	if !strings.Contains(body, `"arguments":"{}"`) {
		t.Fatal("blank arguments were not repaired")
	}
}

func TestStreamingEOFRequiresTerminalEvent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\"}\n\n")
	}))
	defer upstream.Close()
	server := httptest.NewServer(testProxy(t, upstream))
	defer server.Close()
	resp, err := http.Post(
		server.URL+"/v1/responses",
		"application/json",
		strings.NewReader(`{"model":"gpt-test","input":[],"stream":true}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Fatalf("truncated stream ended cleanly without terminal event: HTTP %d, body %q", resp.StatusCode, body)
	}
}

func TestStreamingStopsAfterTerminalEvent(t *testing.T) {
	start := time.Now()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, completedSSE)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	p := testProxy(t, upstream)
	p.cfg.IdleTimeout = 10 * time.Second
	server := httptest.NewServer(p)
	defer server.Close()
	resp, err := http.Post(
		server.URL+"/v1/responses",
		"application/json",
		strings.NewReader(`{"model":"gpt-test","input":[],"stream":true}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("completed stream failed after terminal event: %v; body %q", err, body)
	}
	if !strings.Contains(string(body), "response.completed") {
		t.Fatalf("missing completed event in body: %q", string(body))
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("terminal event waited for idle timeout: elapsed %s", elapsed)
	}
}

func TestEarlyRejectionOfUnreadBodies(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	defer upstream.Close()
	p := testProxy(t, upstream)
	p.cfg.APIKey = "required-key"
	server := httptest.NewUnstartedServer(p)
	server.Config.ReadTimeout = 2 * time.Second
	server.Start()
	defer server.Close()

	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	_, err = fmt.Fprintf(
		conn,
		"POST /v1/responses HTTP/1.1\r\nHost: test\r\nContent-Length: 100\r\n\r\n",
	)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("early rejection waited for unauthenticated body: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}
