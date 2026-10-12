package proxy

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
	if len(items) != 2 {
		t.Fatalf("completed output count=%d, want 2", len(items))
	}
	if jsonString(items[0]["encrypted_content"]) != "opaque" {
		t.Fatal("reasoning output lost or order changed")
	}
	if got := jsonString(items[1]["call_id"]); got != "c1" {
		t.Fatalf("tool call ID=%q, want c1", got)
	}
	if got := jsonString(response["id"]); got != "resp_1" {
		t.Fatalf("response ID=%q, want resp_1", got)
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
	if err != nil {
		t.Fatalf("terminal response collection failed: %v", err)
	}
	if !strings.Contains(string(body), "terminal") {
		t.Fatalf("authoritative terminal item missing: %s", body)
	}
	if strings.Contains(string(body), "ignored") {
		t.Fatal("terminal response was overwritten with retained items")
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
		if err != nil {
			t.Fatalf("native %s terminal response rejected: %v", status, err)
		}
		if !strings.Contains(string(body), status) {
			t.Fatalf("terminal status=%q missing from response", status)
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
