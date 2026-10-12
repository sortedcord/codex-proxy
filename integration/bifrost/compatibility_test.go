package compatibility

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/providers/openai"
	"github.com/maximhq/bifrost/core/schemas"
)

type quietLogger struct{}

func (quietLogger) Debug(string, ...any)                   {}
func (quietLogger) Info(string, ...any)                    {}
func (quietLogger) Warn(string, ...any)                    {}
func (quietLogger) Error(string, ...any)                   {}
func (quietLogger) Fatal(string, ...any)                   {}
func (quietLogger) SetLevel(schemas.LogLevel)              {}
func (quietLogger) SetOutputType(schemas.LoggerOutputType) {}
func (quietLogger) LogHTTPRequest(schemas.LogLevel, string) schemas.LogEventBuilder {
	return schemas.NoopLogEvent
}

func providerForTest(t *testing.T) (*openai.OpenAIProvider, schemas.Key) {
	t.Helper()
	baseURL := os.Getenv("BIFROST_TEST_BASE_URL")
	if baseURL == "" {
		t.Skip("run via CODEX_PROXY_BIFROST_TEST=1 go test in the repository root")
	}
	body, err := os.ReadFile("../../examples/bifrost.config.json")
	if err != nil {
		t.Fatal(err)
	}
	document := struct {
		Providers map[string]json.RawMessage `json:"providers"`
	}{Providers: map[string]json.RawMessage{}}
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	cfg := &schemas.ProviderConfig{}
	if err := json.Unmarshal(document.Providers["codex-proxy"], cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.CustomProviderConfig == nil || cfg.OpenAIConfig == nil {
		t.Fatal("example must configure an OpenAI-compatible custom provider")
	}
	cfg.CustomProviderConfig.CustomProviderKey = "codex-proxy"
	// The gateway registers configured provider prefixes before parsing ingress.
	schemas.RegisterKnownProvider("codex-proxy")
	allowed := cfg.CustomProviderConfig.AllowedRequests
	if allowed == nil || !allowed.Responses || !allowed.ResponsesStream {
		t.Fatal("example does not allow both Responses modes")
	}
	if allowed.ChatCompletion || allowed.ResponsesRetrieve || allowed.ResponsesCancel {
		t.Fatal("example advertises unsupported operations")
	}
	if !cfg.OpenAIConfig.DisableStore {
		t.Fatal("example must disable upstream storage")
	}
	cfg.NetworkConfig.BaseURL = baseURL // Bifrost itself adds /v1/responses.
	cfg.NetworkConfig.ExtraHeaders = map[string]string{"session_id": "bifrost-session"}
	cfg.SendBackRawRequest = true
	t.Setenv("CODEX_PROXY_API_KEY", os.Getenv("BIFROST_TEST_API_KEY"))
	account := struct {
		Keys []schemas.Key `json:"keys"`
	}{Keys: []schemas.Key{}}
	if err := json.Unmarshal(document.Providers["codex-proxy"], &account); err != nil || len(account.Keys) != 1 {
		t.Fatal("example provider key cannot be decoded")
	}
	if account.Keys[0].Value.GetValue() != os.Getenv("BIFROST_TEST_API_KEY") {
		t.Fatal("example provider key did not resolve its environment reference")
	}
	return openai.NewOpenAIProvider(cfg, quietLogger{}), account.Keys[0]
}

func contextForTest(t *testing.T) *schemas.BifrostContext {
	t.Helper()
	ctx, cancel := schemas.NewBifrostContextWithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func requestFromJSON(t *testing.T, ctx *schemas.BifrostContext, body string) *schemas.BifrostResponsesRequest {
	t.Helper()
	wire := openai.OpenAIResponsesRequest{}
	if err := json.Unmarshal([]byte(body), &wire); err != nil {
		t.Fatal(err)
	}
	if model := os.Getenv("CODEX_PROXY_TEST_MODEL"); model != "" && os.Getenv("BIFROST_TEST_LIVE") == "true" {
		wire.Model = "codex-proxy/" + model
	}
	return wire.ToBifrostResponsesRequest(ctx)
}

func textFromResponse(response *schemas.BifrostResponsesResponse) string {
	var text strings.Builder
	for _, item := range response.Output {
		if item.Content == nil {
			continue
		}
		for _, block := range item.Content.ContentBlocks {
			if block.Text != nil {
				text.WriteString(*block.Text)
			}
		}
	}
	return text.String()
}

func TestListModels(t *testing.T) {
	provider, key := providerForTest(t)
	response, failure := provider.ListModels(
		contextForTest(t),
		[]schemas.Key{key},
		&schemas.BifrostListModelsRequest{Provider: "codex-proxy", Unfiltered: true},
	)
	if failure != nil {
		t.Fatalf("Bifrost model discovery failed: %v", failure)
	}
	if len(response.Data) == 0 || !strings.HasPrefix(response.Data[0].ID, "codex-proxy/") {
		t.Fatal("Bifrost model listing lost its provider prefix")
	}
}

func TestResponsesImageInput(t *testing.T) {
	provider, key := providerForTest(t)
	ctx := contextForTest(t)
	picture := image.NewRGBA(image.Rect(
		0,
		0,
		16,
		16,
	))
	for y := range 16 {
		for x := range 16 {
			picture.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, picture); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"model":     "codex-proxy/gpt-5.5",
		"reasoning": map[string]string{"effort": "low"},
		"input": []map[string]any{{
			"role": "user", "content": []map[string]string{
				{"type": "input_text", "text": "bifrost-image: Reply with exactly BIFROST_OK."},
				{"type": "input_image", "image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(encoded.Bytes())},
			},
		}},
	})
	response, failure := provider.Responses(ctx, key, requestFromJSON(t, ctx, string(body)))
	if failure != nil {
		t.Fatalf("Bifrost image input failed: %v", failure)
	}
	if !strings.Contains(textFromResponse(response), "BIFROST_OK") {
		t.Fatal("Bifrost image request could not complete")
	}
}

func TestResponsesNonStreaming(t *testing.T) {
	provider, key := providerForTest(t)
	ctx := contextForTest(t)
	request := requestFromJSON(t, ctx, `{
		"model":"codex-proxy/gpt-5.5","input":"bifrost-text: Reply with exactly BIFROST_OK.",
		"max_output_tokens":128,"temperature":0.3,"top_p":0.8,"user":"test-user",
		"store":true,"reasoning":{"effort":"low"},"metadata":{"test":"bifrost"}
	}`)
	response, failure := provider.Responses(ctx, key, request)
	if failure != nil {
		t.Fatalf("Bifrost non-streaming failed: %v", failure)
	}
	if response.ID == nil || response.Object != "response" || response.Usage == nil {
		t.Fatal("Bifrost could not decode response identity or usage")
	}
	if !strings.Contains(textFromResponse(response), "BIFROST_OK") {
		t.Fatal("Bifrost output text was lost")
	}
	if response.Metadata == nil || (*response.Metadata)["test"] != "bifrost" {
		t.Fatal("Bifrost response metadata was lost")
	}
	if os.Getenv("BIFROST_TEST_LIVE") != "true" && response.Usage.InputTokens != 12 {
		t.Fatal("native usage was changed")
	}
}

func TestResponsesStructuredOutput(t *testing.T) {
	provider, key := providerForTest(t)
	ctx := contextForTest(t)
	request := requestFromJSON(t, ctx, `{
		"model":"codex-proxy/gpt-5.5","input":"bifrost-json: Respond with a JSON object where ok is true.",
		"reasoning":{"effort":"low"},"text":{"format":{
			"type":"json_schema","name":"marker","strict":true,
			"schema":{"type":"object","properties":{"ok":{"type":"boolean"}},
				"required":["ok"],"additionalProperties":false}
		}}
	}`)
	response, failure := provider.Responses(ctx, key, request)
	if failure != nil {
		t.Fatalf("Bifrost structured output failed: %v", failure)
	}
	result := struct {
		OK bool `json:"ok"`
	}{}
	if err := json.Unmarshal([]byte(textFromResponse(response)), &result); err != nil || !result.OK {
		t.Fatal("Bifrost structured-output schema was not honored")
	}
}

func identityHook(
	_ *schemas.BifrostContext,
	response *schemas.BifrostResponse,
	failure *schemas.BifrostError,
) (*schemas.BifrostResponse, *schemas.BifrostError) {
	return response, failure
}

func TestResponsesStreaming(t *testing.T) {
	provider, key := providerForTest(t)
	ctx := contextForTest(t)
	request := requestFromJSON(t, ctx, `{
		"model":"codex-proxy/gpt-5.5","input":"bifrost-text: Reply with exactly BIFROST_OK.",
		"max_output_tokens":128,"store":false,"reasoning":{"effort":"low"},
		"stream_options":{"include_obfuscation":false},"metadata":{"stream_test":"bifrost"}
	}`)
	stream, failure := provider.ResponsesStream(
		ctx,
		identityHook,
		nil,
		key,
		request,
	)
	if failure != nil {
		t.Fatalf("Bifrost streaming setup failed: %v", failure)
	}
	completed := false
	sawOutput := false
	lastSequence := -1
	for chunk := range stream {
		if chunk.BifrostError != nil {
			t.Fatalf("Bifrost stream error: %v", chunk.BifrostError)
		}
		event := chunk.BifrostResponsesStreamResponse
		if event == nil {
			t.Fatal("Bifrost could not decode an SSE event")
		}
		if event.SequenceNumber < lastSequence {
			t.Fatal("Bifrost stream sequence numbers changed order")
		}
		lastSequence = event.SequenceNumber
		if event.Type == schemas.ResponsesStreamResponseTypeOutputItemDone {
			sawOutput = true
		}
		if event.Type == schemas.ResponsesStreamResponseTypeCompleted {
			completed = true
			if event.Response == nil || event.Response.Usage == nil || len(event.Response.Output) == 0 {
				t.Fatal("Bifrost terminal response lost usage or output")
			}
			metadata := event.Response.Metadata
			if metadata == nil || (*metadata)["stream_test"] != "bifrost" {
				t.Fatal("Bifrost terminal response lost metadata")
			}
		}
	}
	if !completed || !sawOutput {
		t.Fatal("Bifrost stream lost output or its terminal event")
	}
}

func TestResponsesStreamErrors(t *testing.T) {
	if os.Getenv("BIFROST_TEST_LIVE") == "true" {
		t.Skip("do not deliberately trigger a live error")
	}
	provider, key := providerForTest(t)
	ctx := contextForTest(t)
	request := requestFromJSON(t, ctx, `{"model":"codex-proxy/gpt-5.5","input":"bifrost-inband-error"}`)
	stream, failure := provider.ResponsesStream(
		ctx,
		identityHook,
		nil,
		key,
		request,
	)
	if failure != nil {
		t.Fatalf("stream setup failed: %v", failure)
	}
	sawError := false
	for chunk := range stream {
		if chunk.BifrostError == nil {
			continue
		}
		sawError = true
		detail := chunk.BifrostError.Error
		if detail == nil || detail.Code == nil || *detail.Code != "context_length_exceeded" {
			t.Fatal("Bifrost lost the native in-band error code")
		}
	}
	if !sawError {
		t.Fatal("Bifrost silently accepted an in-band error")
	}
}

func TestResponsesToolRoundTrip(t *testing.T) {
	provider, key := providerForTest(t)
	ctx := contextForTest(t)
	request := requestFromJSON(t, ctx, `{
		"model":"codex-proxy/gpt-5.5","input":[{"role":"user", "content":
			"bifrost-tools: Call record_marker with value BIFROST_OK."}],
		"include":["reasoning.encrypted_content"],"reasoning":{"effort":"low"},
		"tool_choice":{"type":"function","name":"record_marker"},
		"tools":[{"type":"function","name":"record_marker","description":"Record a marker.",
			"strict":true,"parameters":{"type":"object","properties":{"value":{"type":"string"}},
				"required":["value"],"additionalProperties":false}}]
	}`)
	response, failure := provider.Responses(ctx, key, request)
	if failure != nil {
		t.Fatalf("Bifrost tool request failed: %v", failure)
	}
	var callID string
	for _, item := range response.Output {
		if item.Type != nil && *item.Type == schemas.ResponsesMessageTypeFunctionCall && item.CallID != nil {
			callID = *item.CallID
		}
	}
	if callID == "" {
		t.Fatal("Bifrost tool call identity was lost")
	}
	request.Input = append(request.Input, response.Output...)
	toolResult := []schemas.ResponsesMessage{}
	body, _ := json.Marshal([]map[string]string{
		{"type": "function_call_output", "call_id": callID, "output": "Recorded BIFROST_OK."},
		{"role": "user", "content": "Now reply with exactly BIFROST_OK; do not call another tool."},
	})
	if err := json.Unmarshal(body, &toolResult); err != nil {
		t.Fatal(err)
	}
	request.Input = append(request.Input, toolResult...)
	request.Params.ToolChoice = &schemas.ResponsesToolChoice{ResponsesToolChoiceStr: schemas.Ptr("auto")}
	response, failure = provider.Responses(ctx, key, request)
	if failure != nil {
		t.Fatalf("Bifrost tool replay failed: %v", failure)
	}
	if !strings.Contains(textFromResponse(response), "BIFROST_OK") {
		t.Fatal("Bifrost tool result was not continued")
	}
}

func TestResponsesErrors(t *testing.T) {
	if os.Getenv("BIFROST_TEST_LIVE") == "true" {
		t.Skip("do not deliberately trigger live quota or context errors")
	}
	provider, key := providerForTest(t)
	for _, prompt := range []string{"bifrost-rate-limit", "bifrost-inband-error"} {
		ctx := contextForTest(t)
		request := requestFromJSON(t, ctx, `{"model":"codex-proxy/gpt-5.5","input":"`+prompt+`"}`)
		_, failure := provider.Responses(ctx, key, request)
		want := 429
		if prompt == "bifrost-inband-error" {
			want = 400
		}
		if failure == nil || failure.StatusCode == nil {
			t.Fatal("Bifrost did not receive an error status")
		}
		if *failure.StatusCode != want || failure.Error == nil || failure.Error.Code == nil {
			t.Fatalf("Bifrost error lost its status or code: %v", failure)
		}
	}
	ctx := contextForTest(t)
	request := requestFromJSON(t, ctx, `{"model":"codex-proxy/gpt-5.5","input":"test"}`)
	key.Value = *schemas.NewSecretVar("wrong-proxy-key")
	_, failure := provider.Responses(ctx, key, request)
	if failure == nil || failure.Error == nil || failure.Error.Type == nil {
		t.Fatal("Bifrost did not receive an authentication error")
	}
	if *failure.Error.Type != "authentication_error" {
		t.Fatalf("unexpected Bifrost auth error type: %q", *failure.Error.Type)
	}
}
