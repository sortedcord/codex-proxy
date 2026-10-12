package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestCollectResponseRebuildsOrderedNativeOutput(t *testing.T) {
	stream := strings.Join([]string{
		": keepalive\r\n\r\n",
		`event: response.created` + "\n" + `data: {"type":"response.created"}` + "\n\n",
		`data: {"type":"response.output_item.done","output_index":1,` + "\n" +
			`data: "item":{"type":"function_call","call_id":"c1","name":"shell","arguments":"{}"}}` + "\n\n",
		`data: {"type":"response.output_item.done","output_index":0,` +
			`"item":{"type":"reasoning","encrypted_content":"opaque"}}` + "\n\n",
		`data: {"type":"response.completed","response":{` +
			`"id":"resp_1","object":"response","status":"completed","output":[],` +
			`"usage":{"input_tokens":9,"output_tokens":2}}}`,
	}, "")
	body, err := collectResponse(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	response := map[string]json.RawMessage{}
	_ = json.Unmarshal(body, &response)
	items := []map[string]json.RawMessage{}
	_ = json.Unmarshal(response["output"], &items)
	if len(items) != 2 || jsonString(items[0]["encrypted_content"]) != "opaque" {
		t.Fatal("reasoning output lost or order changed")
	}
	if jsonString(items[1]["call_id"]) != "c1" || jsonString(response["id"]) != "resp_1" {
		t.Fatal("tool call or response identity lost")
	}
	if string(response["usage"]) != `{"input_tokens":9,"output_tokens":2}` {
		t.Fatal("usage lost")
	}
}

func TestCollectResponseKeepsTerminalOutput(t *testing.T) {
	stream := `data: {"type":"response.output_item.done","output_index":0,` +
		`"item":{"type":"message","id":"ignored"}}` + "\n\n" +
		`data: {"type":"response.completed","response":{"output":[{"type":"message","id":"terminal"}]}}` + "\n\n"
	body, err := collectResponse(strings.NewReader(stream))
	correctOutput := strings.Contains(string(body), "terminal") && !strings.Contains(string(body), "ignored")
	if err != nil || !correctOutput {
		t.Fatalf("terminal output not authoritative: %s, %v", body, err)
	}
}

func TestCollectResponseFailureModes(t *testing.T) {
	for _, stream := range []string{
		"", "data: [DONE]\n\n", "data: not-json\n\n",
		"data: {\"type\":\"response.created\"}\n\n",
		"data: {\"type\":\"error\",\"error\":{\"code\":\"usage_limit_reached\"}}\n\n",
		"data: {\"type\":\"response.completed\",\"response\":null}\n\n",
		"data: {\"type\":\"response.output_item.done\"}\n\n",
	} {
		if _, err := collectResponse(strings.NewReader(stream)); err == nil {
			t.Errorf("invalid stream accepted: %q", stream)
		}
	}
}

func TestTerminalFailureAndIncompleteAreNativeResponses(t *testing.T) {
	for _, status := range []string{"failed", "incomplete"} {
		stream := fmt.Sprintf(
			"data: {\"type\":\"response.%s\",\"response\":{\"id\":\"resp_1\",\"status\":\"%s\",\"output\":[]}}\n\n",
			status,
			status,
		)
		body, err := collectResponse(strings.NewReader(stream))
		if err != nil || !strings.Contains(string(body), status) {
			t.Fatalf("native terminal response lost: %v", err)
		}
	}
}

func TestSSELimitsAndLongLines(t *testing.T) {
	stream := "data: " + strings.Repeat("x", 128<<10) + "\n\n"
	if err := readSSE(strings.NewReader(stream), func(data []byte) (bool, error) {
		if len(data) != 128<<10 {
			t.Error("long event truncated")
		}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	oversized := "data: " + strings.Repeat("x", maxEventBytes+1) + "\n\n"
	if err := readSSE(strings.NewReader(oversized), func([]byte) (bool, error) { return false, nil }); err == nil {
		t.Fatal("oversized event accepted")
	}
}

func FuzzCollectResponse(f *testing.F) {
	f.Add("data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n")
	f.Add(":comment\r\ndata: [DONE]\r\n\r\n")
	f.Fuzz(func(t *testing.T, stream string) {
		body, err := collectResponse(strings.NewReader(stream))
		if err == nil && !json.Valid(body) {
			t.Fatal("collector returned invalid JSON")
		}
	})
}
