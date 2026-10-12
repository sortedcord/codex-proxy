package proxy

import (
	"encoding/json"
	"testing"
)

func TestPrepareResponsesRequest(t *testing.T) {
	body := []byte(`{
		"model":"gpt-test", "input":"hello", "stream":false, "store":true,
		"temperature":0.5, "max_output_tokens":5, "service_tier":"fast",
		"include":["output.encrypted_content"], "parallel_tool_calls":false,
		"tools":[{"type":"function","name":"test","parameters":{"type":"object"}}],
		"unknown":{"nested":9007199254740993}
	}`)
	prepared, err := prepareRequest(body, false)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.stream {
		t.Fatal("client stream=false preference was not preserved")
	}
	if prepared.model != "gpt-test" {
		t.Fatalf("prepared model=%q, want gpt-test", prepared.model)
	}
	if prepared.tier != "priority" {
		t.Fatalf("prepared tier=%q, want priority", prepared.tier)
	}
	object := map[string]json.RawMessage{}
	_ = json.Unmarshal(prepared.body, &object)
	for _, field := range []string{"temperature", "max_output_tokens"} {
		if _, ok := object[field]; ok {
			t.Errorf("unsupported field %s retained", field)
		}
	}
	if string(object["stream"]) != "true" {
		t.Fatalf("backend stream=%s, want true", object["stream"])
	}
	if string(object["store"]) != "false" {
		t.Fatalf("backend store=%s, want false", object["store"])
	}
	if string(object["parallel_tool_calls"]) != "false" {
		t.Fatal("explicit parallel_tool_calls=false changed")
	}
	if string(object["include"]) != `["output.encrypted_content","reasoning.encrypted_content"]` {
		t.Fatal("encrypted replay include lost")
	}
	if string(object["instructions"]) != `""` {
		t.Fatal("instructions default missing")
	}
	if string(object["unknown"]) != `{"nested":9007199254740993}` {
		t.Fatal("raw extension field lost numeric precision")
	}
}

func TestNormalizeHistoryPreservesNativeItems(t *testing.T) {
	prepared, err := prepareRequest([]byte(`{
		"model":"gpt-test", "input":[
			{"role":"system","content":"rules"},
			{"type":"reasoning","id":"rs_1","encrypted_content":"opaque"},
			{"type":"function_call","name":"noop","call_id":"call_1","arguments":""},
			{"type":"function_call_output","call_id":"call_1","output":"ok"},
			{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AA=="}]}
		]
	}`), false)
	if err != nil {
		t.Fatal(err)
	}
	object := map[string]json.RawMessage{}
	_ = json.Unmarshal(prepared.body, &object)
	items := []map[string]json.RawMessage{}
	if err := json.Unmarshal(object["input"], &items); err != nil {
		t.Fatalf("normalized input could not be decoded: %v", err)
	}
	if len(items) != 5 {
		t.Fatalf("normalized input count=%d, want 5", len(items))
	}
	if jsonString(items[0]["role"]) != "developer" {
		t.Fatal("system role not normalized")
	}
	if jsonString(items[1]["encrypted_content"]) != "opaque" {
		t.Fatal("native encrypted reasoning changed")
	}
	if got := jsonString(items[2]["arguments"]); got != "{}" {
		t.Fatalf("normalized tool arguments=%q, want {}", got)
	}
	if jsonString(items[3]["output"]) != "ok" {
		t.Fatal("tool result lost")
	}
}

func TestCompactDoesNotUseStreamingFlags(t *testing.T) {
	prepared, err := prepareRequest([]byte(`{"model":"gpt-test","input":[],"stream":false,"store":true}`), true)
	if err != nil {
		t.Fatal(err)
	}
	object := map[string]json.RawMessage{}
	_ = json.Unmarshal(prepared.body, &object)
	for _, field := range []string{"stream", "store", "include", "parallel_tool_calls"} {
		if _, ok := object[field]; ok {
			t.Errorf("compact contains %s", field)
		}
	}
}

func TestRequestValidation(t *testing.T) {
	cases := []string{
		`null`, `[]`, `{}`, `{"model":1,"input":"x"}`,
		`{"model":"gpt-test"}`, `{"model":"gpt-test","input":null}`,
		`{"model":"gpt-test","input":[null]}`, `{"model":"gpt-test","input":{},"stream":true}`,
		`{"model":"gpt-test","input":"x","stream":"true"}`,
		`{"model":"gpt-test","input":"x","stream":null}`,
		`{"model":"gpt-test","input":"x","instructions":[]}`,
		`{"model":"gpt-test","input":"x","include":{}}`,
		`{"model":"gpt-test","input":"x","previous_response_id":"resp_1"}`,
		`{"model":"gpt-test","input":"x","background":true}`,
		`{"model":"gpt-test","input":"x"} {"extra":true}`,
	}
	for _, body := range cases {
		t.Run(body, func(t *testing.T) {
			if _, err := prepareRequest([]byte(body), false); err == nil {
				t.Fatal("invalid request accepted")
			}
		})
	}
	if _, err := prepareRequest([]byte(`{"model":"gpt-test","input":[],"stream":true}`), true); err == nil {
		t.Fatal("streaming compaction accepted")
	}
}

func FuzzPrepareRequest(f *testing.F) {
	f.Add([]byte(`{"model":"gpt-test","input":"hello"}`), false)
	f.Add([]byte(`{"model":"gpt-test","input":[]}`), true)
	f.Fuzz(func(t *testing.T, body []byte, compact bool) {
		prepared, err := prepareRequest(body, compact)
		if err == nil && !json.Valid(prepared.body) {
			t.Fatal("normalization produced invalid JSON")
		}
	})
}
