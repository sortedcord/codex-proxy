package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sortedcord/codex-proxy/internal/config"
	"github.com/sortedcord/codex-proxy/internal/oauth"
)

// Explicitly opt in: this test uses real credentials and subscription quota.
func TestLiveProxy(t *testing.T) {
	if os.Getenv("CODEX_PROXY_LIVE_TEST") != "1" {
		t.Skip("set CODEX_PROXY_LIVE_TEST=1 to use installed Codex credentials")
	}
	cfg, err := config.Parse([]string{}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	client := newHTTPClient()
	defer client.CloseIdleConnections()
	auth, err := oauth.NewManager(cfg.AuthFile, client, oauth.DefaultTokenURL)
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
	model := os.Getenv("CODEX_PROXY_TEST_MODEL")
	if model == "" {
		model = "gpt-5.5"
	}

	call := func(t *testing.T, method, path string, body []byte) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, server.URL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 {
			_ = resp.Body.Close()
			t.Fatalf("%s returned HTTP %d", path, resp.StatusCode)
		}
		return resp
	}

	t.Run("models", func(t *testing.T) {
		resp := call(
			t,
			"GET",
			"/v1/models",
			[]byte{},
		)
		defer resp.Body.Close()
		catalog := struct {
			Data []json.RawMessage `json:"data"`
		}{Data: []json.RawMessage{}}
		if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil || len(catalog.Data) == 0 {
			t.Fatal("live model catalog unavailable")
		}
	})
	for _, stream := range []bool{false, true} {
		name := "nonstream"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{
				"model": model, "input": "Reply with exactly PROXY_OK.", "stream": stream,
				"reasoning": map[string]string{"effort": "low"},
			})
			resp := call(
				t,
				"POST",
				"/v1/responses",
				body,
			)
			defer resp.Body.Close()
			result, err := readBounded(resp.Body, maxJSONBytes)
			if err != nil {
				t.Fatal(err)
			}
			if stream {
				if !bytes.Contains(result, []byte("response.completed")) {
					t.Fatal("live stream missing completion")
				}
				return
			}
			if !json.Valid(result) || !bytes.Contains(result, []byte("PROXY_OK")) {
				t.Fatal("live response missing expected output")
			}
		})
	}
	if os.Getenv("CODEX_PROXY_TEST_CODEX") == "1" {
		t.Run("codex_cli", func(t *testing.T) {
			args := []string{
				"exec", "--ignore-user-config", "--ignore-rules", "--ephemeral",
				"--skip-git-repo-check", "-s", "read-only", "-m", model,
				"-c", `model_provider="codex_proxy"`,
				"-c", `model_providers.codex_proxy.name="codex-proxy"`,
				"-c", `model_providers.codex_proxy.base_url="` + server.URL + `/v1"`,
				"-c", `model_providers.codex_proxy.wire_api="responses"`,
				"-c", `model_providers.codex_proxy.supports_websockets=false`,
				"-c", `model_providers.codex_proxy.requires_openai_auth=false`,
				"-c", `model_reasoning_effort="low"`,
			}
			if cfg.APIKey != "" {
				args = append(args, "-c", `model_providers.codex_proxy.env_key="CODEX_PROXY_API_KEY"`)
			}
			args = append(args, "Do not use tools. Reply with exactly CODEX_PROXY_OK.")
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			output, err := exec.CommandContext(ctx, "codex", args...).CombinedOutput()
			if err != nil {
				t.Fatalf("codex CLI failed: %v (output suppressed)", err)
			}
			if !strings.Contains(string(output), "CODEX_PROXY_OK") {
				t.Fatal("codex CLI missing expected output")
			}
		})
	}
}
