package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The real Bifrost provider lives in a separate test-only module. Neither normal
// tests nor the production binary acquire its dependencies or real credentials.
func TestBifrostCompatibility(t *testing.T) {
	if os.Getenv("CODEX_PROXY_BIFROST_TEST") != "1" {
		t.Skip("set CODEX_PROXY_BIFROST_TEST=1 to run the real Bifrost provider")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			writeJSON(w, 200, []byte(`{"models":[{"slug":"gpt-5.5"}]}`))
			return
		}
		if r.URL.Path != "/responses" {
			t.Errorf("unexpected upstream path %q", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		object := map[string]json.RawMessage{}
		if err := json.NewDecoder(r.Body).Decode(&object); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if string(object["stream"]) != "true" || string(object["store"]) != "false" {
			t.Error("Bifrost request did not normalize Codex flags")
		}
		if r.Header.Get("Authorization") == "Bearer bifrost-test-key" {
			t.Error("Bifrost local key leaked to upstream")
		}
		if r.Header.Get("Session_id") != "bifrost-session" {
			t.Error("Bifrost forwarded session affinity header was lost")
		}
		for _, field := range []string{"max_output_tokens", "temperature", "top_p", "user", "metadata"} {
			if _, ok := object[field]; ok {
				t.Errorf("unsupported Bifrost field %q reached Codex", field)
			}
		}
		input := string(object["input"])
		if strings.Contains(input, "bifrost-rate-limit") {
			w.Header().Set("Retry-After", "42")
			writeJSON(w, 429, []byte(
				`{"error":{"type":"rate_limit_error","code":"usage_limit_reached","message":"quota"}}`,
			))
			return
		}
		if strings.Contains(input, "bifrost-inband-error") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w,
				"data: {\"type\":\"error\",\"code\":\"context_length_exceeded\",\"message\":\"too long\"}\n\n",
			)
			return
		}
		text := "BIFROST_OK"
		if strings.Contains(input, "bifrost-json") {
			text = `{"ok":true}`
			if !strings.Contains(string(object["text"]), "json_schema") {
				t.Error("Bifrost structured-output schema was lost")
			}
		}
		output := []map[string]any{
			{
				"type": "message", "id": "msg_test", "role": "assistant", "status": "completed",
				"content": []map[string]any{{"type": "output_text", "text": text, "annotations": []any{}}},
			},
		}
		if strings.Contains(input, "bifrost-tools") && !strings.Contains(input, "function_call_output") {
			output = []map[string]any{
				{"type": "reasoning", "id": "rs_test", "summary": []any{}, "encrypted_content": "opaque-test"},
				{
					"type": "function_call", "id": "fc_test", "call_id": "call_test", "name": "record_marker",
					"arguments": `{"value":"BIFROST_OK"}`, "status": "completed",
				},
			}
		}
		if strings.Contains(input, "function_call_output") && !strings.Contains(input, "opaque-test") {
			t.Error("encrypted reasoning was lost during Bifrost tool replay")
		}
		response := map[string]any{
			"id": "resp_test", "object": "response", "created_at": 1, "model": "gpt-5.5",
			"status": "completed", "output": []any{}, "store": false,
			"usage": map[string]any{
				"input_tokens": 12, "output_tokens": 3, "total_tokens": 15,
				"input_tokens_details":  map[string]int{"cached_tokens": 4},
				"output_tokens_details": map[string]int{"reasoning_tokens": 1},
			},
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", "bifrost-request")
		for index, item := range output {
			event, _ := json.Marshal(map[string]any{
				"type": "response.output_item.done", "sequence_number": index,
				"output_index": index, "item": item,
			})
			_, _ = fmt.Fprintf(w, "event: response.output_item.done\ndata: %s\n\n", event)
		}
		// Empty terminal output deliberately exercises collector reconstruction.
		terminal, _ := json.Marshal(map[string]any{
			"type": "response.completed", "sequence_number": len(output), "response": response,
		})
		_, _ = fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", terminal)
	}))
	defer upstream.Close()
	p := testProxy(t, upstream)
	p.cfg.apiKey = "bifrost-test-key"
	server := httptest.NewServer(p)
	defer server.Close()
	runBifrostTests(
		t,
		server.URL,
		p.cfg.apiKey,
		false,
	)
}

func TestBifrostLive(t *testing.T) {
	if os.Getenv("CODEX_PROXY_BIFROST_LIVE_TEST") != "1" {
		t.Skip("set CODEX_PROXY_BIFROST_LIVE_TEST=1 to use real Codex credentials via Bifrost")
	}
	cfg, err := parseConfig([]string{}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	cfg.apiKey = "bifrost-live-test-key"
	client := newHTTPClient()
	defer client.CloseIdleConnections()
	auth, err := newAuthManager(cfg.authFile, client, defaultTokenURL)
	if err != nil {
		t.Fatal(err)
	}
	p := newProxy(
		cfg,
		auth,
		client,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	server := httptest.NewServer(p)
	defer server.Close()
	runBifrostTests(
		t,
		server.URL,
		cfg.apiKey,
		true,
	)
}

func runBifrostTests(t *testing.T, baseURL, key string, live bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	command := exec.CommandContext(
		ctx,
		"go",
		"test",
		"-race",
		"-v",
		"-count=1",
		"-timeout=5m",
		"./...",
	)
	command.Dir = "integration/bifrost"
	command.Env = append(os.Environ(),
		"BIFROST_TEST_BASE_URL="+baseURL,
		"BIFROST_TEST_API_KEY="+key,
		fmt.Sprintf("BIFROST_TEST_LIVE=%t", live),
	)
	output, err := command.CombinedOutput()
	t.Logf("Bifrost provider integration:\n%s", output)
	if err != nil {
		t.Fatalf("Bifrost provider integration failed: %v", err)
	}
}
