package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

type streamError struct {
	body   []byte
	status int
}

func (e *streamError) Error() string {
	return "upstream emitted an error event"
}

func newStreamError(data []byte) *streamError {
	object := map[string]json.RawMessage{}
	_ = json.Unmarshal(data, &object)
	detail := object["error"]
	if len(detail) == 0 || bytes.Equal(detail, []byte("null")) {
		delete(object, "type")
		detail, _ = json.Marshal(object)
	}
	fields := map[string]json.RawMessage{}
	_ = json.Unmarshal(detail, &fields)
	status := streamErrorStatus(jsonString(fields["code"]))
	if status == http.StatusBadGateway {
		status = streamErrorStatus(jsonString(fields["type"]))
	}
	body, _ := json.Marshal(map[string]json.RawMessage{"error": detail})
	return &streamError{body: body, status: status}
}

func streamErrorStatus(signal string) int {
	switch signal {
	case "rate_limit_exceeded", "usage_limit_reached", "rate_limit_error", "too_many_requests", "insufficient_quota":
		return http.StatusTooManyRequests
	case "context_length_exceeded", "invalid_prompt", "invalid_request_error", "invalid_value", "unsupported_parameter":
		return http.StatusBadRequest
	case "invalid_api_key", "authentication_error":
		return http.StatusUnauthorized
	case "server_is_overloaded", "server_overloaded":
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadGateway
	}
}

func isTerminalEventType(eventType string) bool {
	switch eventType {
	case "response.completed", "response.done", "response.incomplete", "response.failed", "error":
		return true
	default:
		return false
	}
}

// collectResponse retains completed output items because Codex may omit them
// from the terminal response object. Deltas are never concatenated into guessed
// messages: tool calls, reasoning, images, and unknown items remain native JSON.
func collectResponse(reader io.Reader) ([]byte, error) {
	output := newResponseOutput()
	var result []byte // Nil signals no terminal event yet, not an API array.
	err := readSSE(reader, func(data []byte) (bool, error) {
		if bytes.Equal(data, []byte("[DONE]")) {
			return true, nil
		}
		event := struct {
			Type        string          `json:"type"`
			OutputIndex int             `json:"output_index"`
			Item        json.RawMessage `json:"item"`
			Response    json.RawMessage `json:"response"`
		}{}
		if err := json.Unmarshal(data, &event); err != nil {
			return false, errors.New("invalid JSON in upstream event stream")
		}
		if event.Type == "response.output_item.done" {
			if err := output.add(event.OutputIndex, event.Item); err != nil {
				return false, err
			}
		}
		if event.Type == "error" {
			return false, newStreamError(data)
		}
		if isTerminalEventType(event.Type) {
			var err error
			result, err = output.complete(event.Response)
			return true, err
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return nil, errors.New("upstream stream ended without a terminal response")
	}
	return result, nil
}

func readSSE(reader io.Reader, consume func([]byte) (bool, error)) error {
	scanner := newSSEScanner(reader)
	for {
		frame, err := scanner.next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if len(frame.data) == 0 {
			continue
		}
		done, err := consume(frame.data)
		if err != nil || done {
			return err
		}
	}
}
