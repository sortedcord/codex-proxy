package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestStreamingTerminalOutputReconstruction(t *testing.T) {
	stream := "data: {\"type\":\"response.output_item.done\",\"output_index\":1," +
		"\"item\":{\"type\":\"function_call\",\"arguments\":\"{}\",\"call_id\":\"call_test\"}}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0," +
		"\"item\":{\"type\":\"reasoning\",\"encrypted_content\":\"opaque\",\"exact\":9007199254740993}}\n\n" +
		"data: {\"type\":\"response.completed\",\"sequence_number\":4," +
		"\"response\":{\"id\":\"response_test\",\"output\":[]}}\n\n"
	reader := newResponseStreamReader(strings.NewReader(stream), nil)
	result, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(result, []byte(stream[:strings.LastIndex(stream, "data:")])) {
		t.Fatal("complete native output-item frames changed")
	}
	response, err := collectResponse(bytes.NewReader(result))
	if err != nil {
		t.Fatal(err)
	}
	object := struct {
		Output []json.RawMessage `json:"output"`
	}{Output: []json.RawMessage{}}
	_ = json.Unmarshal(response, &object)
	if len(object.Output) != 2 || !bytes.Contains(object.Output[0], []byte("9007199254740993")) {
		t.Fatal("streaming terminal output lost order, native fields, or precision")
	}
	if !bytes.Contains(result, []byte(`"sequence_number":4`)) {
		t.Fatal("terminal event sequence number changed")
	}
}

func TestOutputStorageValidation(t *testing.T) {
	output := newResponseOutput()
	if err := output.add(-1, []byte(`{}`)); err == nil {
		t.Fatal("negative output index accepted")
	}
	if err := output.add(0, []byte(`null`)); err == nil {
		t.Fatal("null output accepted")
	}
	output.bytes = maxJSONBytes
	if err := output.add(0, []byte(`{}`)); err == nil {
		t.Fatal("oversized output accepted")
	}
	for _, response := range []string{"null", "[]", `{"output":{}}`} {
		if _, err := output.complete([]byte(response)); err == nil {
			t.Fatal("invalid terminal response accepted")
		}
	}
	output = newResponseOutput()
	_ = output.add(0, []byte(`{"type":"message","id":"should_not_replace"}`))
	native := []byte(`{"output":[{"type":"message","id":"authoritative"}]}`)
	completed, err := output.complete(native)
	if err != nil || !bytes.Equal(completed, native) {
		t.Fatal("nonempty native terminal output was replaced")
	}
}
