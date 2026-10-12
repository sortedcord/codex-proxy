package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	if response.Code != 429 || !strings.Contains(response.Body.String(), "usage_limit_reached") {
		t.Fatal("collected error code/status lost")
	}
	for _, code := range []string{"server_is_overloaded", "unexpected_error"} {
		event := []byte(`{"type":"error","code":"` + code + `","message":"details"}`)
		err := newStreamError(event)
		if !json.Valid(err.body) || strings.Contains(err.Error(), "details") {
			t.Fatal("invalid error body or unsafe log message")
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
	if strings.Contains(body, "prompt_cache_breakpoint") || !strings.Contains(body, `"leave":"unchanged"`) {
		t.Fatal("cache hints retained or non-string arguments silently rewritten")
	}
	if !strings.Contains(body, `"arguments":"{}"`) {
		t.Fatal("blank arguments were not repaired")
	}
}
