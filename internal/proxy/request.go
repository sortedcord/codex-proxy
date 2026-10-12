package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/sortedcord/codex-proxy/internal/oauth"
)

type preparedRequest struct {
	body     []byte
	stream   bool
	model    string
	tier     string
	metadata json.RawMessage
	creds    oauth.Credentials
}

func isNull(raw []byte) bool {
	return len(raw) == 0 || bytes.Equal(raw, []byte("null"))
}

var unsupportedBackendFields = [...]string{
	"max_output_tokens", "max_completion_tokens", "temperature", "top_p", "truncation",
	"prompt_cache_options", "prompt_cache_retention", "user", "context_management",
}

func validateUnsupportedFeatures(object map[string]json.RawMessage) error {
	if raw := object["previous_response_id"]; !isNull(raw) {
		return errors.New("previous_response_id is unsupported over HTTP; send full input history")
	}
	delete(object, "previous_response_id")

	if raw, ok := object["background"]; ok {
		var background bool
		if json.Unmarshal(raw, &background) != nil || background {
			return errors.New("background responses are unsupported")
		}
		delete(object, "background")
	}

	for _, field := range [...]string{"conversation", "prompt"} {
		if raw := object[field]; !isNull(raw) {
			return errors.New(field + " references are unsupported; send full input history and instructions")
		}
		delete(object, field)
	}
	return nil
}

func extractMetadata(object map[string]json.RawMessage) (json.RawMessage, error) {
	raw, ok := object["metadata"]
	if !ok || isNull(raw) {
		delete(object, "metadata")
		return nil, nil
	}
	metadata := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &metadata); err != nil || metadata == nil {
		return nil, errors.New("metadata must be an object")
	}
	// Metadata is client-side bookkeeping, not a Codex backend parameter.
	delete(object, "metadata")
	return bytes.Clone(raw), nil
}

func normalizeStreamOptions(object map[string]json.RawMessage) error {
	raw, ok := object["stream_options"]
	if !ok || isNull(raw) {
		return nil
	}
	options := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &options); err != nil || options == nil {
		return errors.New("stream_options must be an object")
	}
	// Bifrost's documented include_usage hint is not a Codex parameter.
	// Usage already arrives in every native terminal response.
	delete(options, "include_usage")
	if len(options) == 0 {
		delete(object, "stream_options")
	} else {
		setJSON(object, "stream_options", options)
	}
	return nil
}

func applyBackendDefaults(object map[string]json.RawMessage, compact bool) error {
	for _, field := range unsupportedBackendFields {
		delete(object, field)
	}
	if compact {
		delete(object, "stream")
		delete(object, "store")
		return nil
	}
	setJSON(object, "stream", true)
	setJSON(object, "store", false)
	if _, ok := object["parallel_tool_calls"]; !ok {
		setJSON(object, "parallel_tool_calls", true)
	}
	includes := []string{}
	if raw, ok := object["include"]; ok {
		if err := json.Unmarshal(raw, &includes); err != nil {
			return errors.New("include must be an array of strings")
		}
	}
	if !slices.Contains(includes, "reasoning.encrypted_content") {
		includes = append(includes, "reasoning.encrypted_content")
	}
	setJSON(object, "include", includes)
	return nil
}

func prepareRequest(body []byte, compact bool) (preparedRequest, error) {
	prepared := preparedRequest{}
	object := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &object); err != nil || object == nil {
		return prepared, errors.New("body must be a JSON object")
	}
	prepared.model = jsonString(object["model"])
	if strings.TrimSpace(prepared.model) == "" {
		return prepared, errors.New("model must be a non-empty string")
	}
	if raw, ok := object["stream"]; ok {
		if bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, &prepared.stream) != nil {
			return prepared, errors.New("stream must be a boolean")
		}
	}
	if compact && prepared.stream {
		return prepared, errors.New("compaction does not support streaming")
	}
	if err := validateUnsupportedFeatures(object); err != nil {
		return prepared, err
	}
	var err error
	prepared.metadata, err = extractMetadata(object)
	if err != nil {
		return prepared, err
	}
	if err := normalizeStreamOptions(object); err != nil {
		return prepared, err
	}

	input, err := normalizeInput(object["input"])
	if err != nil {
		return prepared, err
	}
	object["input"] = input
	if raw, ok := object["instructions"]; !ok || isNull(raw) {
		setJSON(object, "instructions", "")
	} else {
		var instructions string
		if err := json.Unmarshal(raw, &instructions); err != nil {
			return prepared, errors.New("instructions must be a string")
		}
	}

	prepared.tier = jsonString(object["service_tier"])
	if prepared.tier == "fast" {
		prepared.tier = "priority"
		setJSON(object, "service_tier", prepared.tier)
	}
	if err := applyBackendDefaults(object, compact); err != nil {
		return prepared, err
	}
	prepared.body, err = json.Marshal(object)
	return prepared, err
}

func normalizeInput(raw json.RawMessage) (json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, errors.New("input is required")
	}
	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, errors.New("invalid input string")
		}
		return json.Marshal([]any{
			map[string]any{
				"type": "message", "role": "user",
				"content": []any{map[string]string{"type": "input_text", "text": text}},
			},
		})
	}
	if raw[0] != '[' {
		return nil, errors.New("input must be a string or an array of items")
	}
	items := []json.RawMessage{}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, errors.New("invalid input array")
	}
	for index, item := range items {
		object := map[string]json.RawMessage{}
		if err := json.Unmarshal(item, &object); err != nil || object == nil {
			return nil, errors.New("input items must be objects")
		}
		changed := false
		if jsonString(object["role"]) == "system" {
			setJSON(object, "role", "developer")
			changed = true
		}
		if jsonString(object["type"]) == "function_call" {
			args := bytes.TrimSpace(object["arguments"])
			if len(args) > 0 && args[0] == '"' {
				if strings.TrimSpace(jsonString(args)) == "" {
					setJSON(object, "arguments", "{}")
					changed = true
				}
			}
		}
		if _, ok := object["prompt_cache_breakpoint"]; ok {
			delete(object, "prompt_cache_breakpoint")
			changed = true
		}
		for _, field := range []string{"content", "output"} {
			parts := bytes.TrimSpace(object[field])
			if len(parts) == 0 || parts[0] != '[' {
				continue
			}
			cleaned, modified := stripCacheBreakpoints(parts)
			if modified {
				object[field] = cleaned
				changed = true
			}
		}
		if changed {
			items[index], _ = json.Marshal(object)
		}
	}
	return json.Marshal(items)
}

func stripCacheBreakpoints(raw json.RawMessage) (json.RawMessage, bool) {
	parts := []json.RawMessage{}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return raw, false
	}
	changed := false
	for index, part := range parts {
		object := map[string]json.RawMessage{}
		if err := json.Unmarshal(part, &object); err != nil || object == nil {
			continue
		}
		if _, ok := object["prompt_cache_breakpoint"]; !ok {
			continue
		}
		delete(object, "prompt_cache_breakpoint")
		parts[index], _ = json.Marshal(object)
		changed = true
	}
	if !changed {
		return raw, false
	}
	cleaned, _ := json.Marshal(parts)
	return cleaned, true
}
