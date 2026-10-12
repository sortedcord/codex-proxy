package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBifrostRequestBookkeeping(t *testing.T) {
	prepared, err := prepareRequest([]byte(`{
		"model":"gpt-test","input":"test","metadata":{"tenant":"one","nested":{"exact":9007199254740993}},
		"stream_options":{"include_usage":true,"reasoning_summary_delivery":"sequential_cutoff"}
	}`), false)
	if err != nil {
		t.Fatal(err)
	}
	object := map[string]json.RawMessage{}
	_ = json.Unmarshal(prepared.body, &object)
	if _, ok := object["metadata"]; ok {
		t.Fatal("client metadata was sent to Codex")
	}
	if !bytes.Contains(prepared.metadata, []byte("9007199254740993")) {
		t.Fatal("metadata lost numeric precision")
	}
	if string(object["stream_options"]) != `{"reasoning_summary_delivery":"sequential_cutoff"}` {
		t.Fatal("Bifrost usage option was not stripped or native option was lost")
	}
	for _, field := range []string{"metadata", "stream_options", "conversation", "prompt"} {
		body := `{"model":"gpt-test","input":"test","` + field + `":"unsupported"}`
		if _, err := prepareRequest([]byte(body), false); err == nil {
			t.Errorf("unsupported %q value accepted", field)
		}
	}
}

func TestMetadataResponsesBothModes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte(`"metadata"`)) {
			t.Error("metadata reached upstream")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, completedSSE)
	}))
	defer upstream.Close()
	p := testProxy(t, upstream)
	for _, stream := range []bool{false, true} {
		body, _ := json.Marshal(map[string]any{
			"model": "gpt-test", "input": "test", "stream": stream,
			"metadata": map[string]string{"tenant": "one"},
		})
		response := requestProxy(
			p,
			"POST",
			"/v1/responses",
			string(body),
		)
		if response.Code != http.StatusOK {
			t.Fatalf("metadata response status=%d, want 200", response.Code)
		}
		if !strings.Contains(response.Body.String(), `"metadata":{"tenant":"one"}`) {
			t.Fatal("client metadata not echoed in response")
		}
	}
}

func TestMetadataReaderPreservesFraming(t *testing.T) {
	frame := "event: response.completed\r\nid: last\r\nretry: 100\r\n" +
		"data: {\"type\":\"response.completed\",\"sequence_number\":9,\r\n" +
		"data: \"response\":{\"output\":[],\"metadata\":{\"upstream\":true}}}\r\n\r\n"
	keepalive := ": heartbeat\r\n\r\n"
	delta := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"
	ending := "data: [DONE]\n\n"
	reader := newResponseStreamReader(strings.NewReader(keepalive+delta+frame+ending), []byte(`{"tenant":"one"}`))
	// Exercise partial reads, so rewritten bytes cannot depend on scanner buffers.
	var output strings.Builder
	buffer := make([]byte, 7)
	for {
		n, err := reader.Read(buffer)
		output.Write(buffer[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	result := output.String()
	if !strings.HasPrefix(result, keepalive+delta) {
		t.Fatalf("keepalive/delta framing changed: %q", result)
	}
	if strings.Contains(result, "[DONE]") {
		t.Fatal("reader consumed data after the native terminal event")
	}
	if !strings.Contains(result, "event: response.completed\r\nid: last\r\nretry: 100\r\n") {
		t.Fatal("SSE event/id/retry fields lost")
	}
	if !strings.Contains(result, `"metadata":{"tenant":"one"}`) {
		t.Fatal("response metadata lost")
	}
	if !strings.Contains(result, `"sequence_number":9`) {
		t.Fatal("event sequence number lost")
	}
}

func TestMetadataReaderLimitsAndMalformedEvents(t *testing.T) {
	for _, frame := range []string{
		"data: not-json\n\n",
		"data: {\"response\":[]}\n\n",
		"data: " + strings.Repeat("x", maxEventBytes+1),
	} {
		reader := newResponseStreamReader(strings.NewReader(frame), []byte(`{}`))
		if _, err := io.ReadAll(reader); err == nil {
			t.Fatal("invalid or oversized metadata stream accepted")
		}
	}
	reader := newResponseStreamReader(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}"), []byte(`{}`))
	if _, err := io.ReadAll(reader); err != nil {
		t.Fatal("final unterminated frame rejected", err)
	}
}

func TestOpenAIErrorTypesAndStatuses(t *testing.T) {
	for _, status := range []int{400, 401, 429, 502} {
		response := httptest.NewRecorder()
		writeError(
			response,
			status,
			"test_error",
			"test message",
		)
		body := struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}{}
		_ = json.Unmarshal(response.Body.Bytes(), &body)
		want := "invalid_request_error"
		switch status {
		case 401:
			want = "authentication_error"
		case 429:
			want = "rate_limit_error"
		case 502:
			want = "server_error"
		}
		if body.Error.Type != want {
			t.Errorf("HTTP %d error type = %q", status, body.Error.Type)
		}
	}
	for signal, status := range map[string]int{
		"context_length_exceeded": 400, "invalid_prompt": 400, "authentication_error": 401,
		"insufficient_quota": 429, "usage_limit_reached": 429, "server_is_overloaded": 503,
	} {
		event := []byte(`{"type":"error","error":{"type":"` + signal + `","message":"details"}}`)
		if got := newStreamError(event).status; got != status {
			t.Errorf(
				"%q status = %d, want %d",
				signal,
				got,
				status,
			)
		}
	}
}

func FuzzMetadataStream(f *testing.F) {
	f.Add("data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n")
	f.Add(":keepalive\r\n\r\ndata: [DONE]\r\n\r\n")
	f.Fuzz(func(t *testing.T, frame string) {
		reader := newResponseStreamReader(strings.NewReader(frame), []byte(`{"tenant":"test"}`))
		_, _ = io.ReadAll(reader) // Untrusted upstream data must never panic.
	})
}
