package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
)

const (
	maxEventBytes = 16 << 20
	maxJSONBytes  = 32 << 20
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
	code := jsonString(fields["code"])
	status := http.StatusBadGateway
	switch code {
	case "rate_limit_exceeded", "usage_limit_reached", "rate_limit_error":
		status = http.StatusTooManyRequests
	case "server_is_overloaded", "server_overloaded":
		status = http.StatusServiceUnavailable
	}
	body, _ := json.Marshal(map[string]json.RawMessage{"error": detail})
	return &streamError{body: body, status: status}
}

// collectResponse retains completed output items because Codex may omit them
// from the terminal response object. Deltas are never concatenated into guessed
// messages: tool calls, reasoning, images, and unknown items remain native JSON.
func collectResponse(reader io.Reader) ([]byte, error) {
	items := map[int]json.RawMessage{}
	var outputBytes int
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
		switch event.Type {
		case "response.output_item.done":
			if len(event.Item) == 0 || bytes.Equal(event.Item, []byte("null")) {
				return false, errors.New("upstream output event missing item")
			}
			outputBytes += len(event.Item) - len(items[event.OutputIndex])
			if outputBytes > maxJSONBytes {
				return false, errors.New("upstream output exceeds size limit")
			}
			items[event.OutputIndex] = bytes.Clone(event.Item)
		case "response.completed", "response.done", "response.incomplete", "response.failed":
			response := map[string]json.RawMessage{}
			if err := json.Unmarshal(event.Response, &response); err != nil || response == nil {
				return false, errors.New("upstream terminal event missing response")
			}
			output := []json.RawMessage{}
			if raw, ok := response["output"]; ok {
				if err := json.Unmarshal(raw, &output); err != nil {
					return false, errors.New("invalid upstream response output")
				}
			}
			if len(output) == 0 {
				indexes := make([]int, 0, len(items))
				for index := range items {
					indexes = append(indexes, index)
				}
				slices.Sort(indexes)
				output = make([]json.RawMessage, 0, len(indexes))
				for _, index := range indexes {
					output = append(output, items[index])
				}
				setJSON(response, "output", output)
			}
			result, _ = json.Marshal(response)
			if len(result) > maxJSONBytes {
				return false, errors.New("upstream response exceeds size limit")
			}
			return true, nil
		case "error":
			return false, newStreamError(data)
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
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), maxEventBytes)
	var data strings.Builder
	dispatch := func() (bool, error) {
		if data.Len() == 0 {
			return false, nil
		}
		payload := strings.TrimSuffix(data.String(), "\n")
		data.Reset()
		return consume([]byte(payload))
	}
	first := true
	for scanner.Scan() {
		line := scanner.Text()
		if first {
			line = strings.TrimPrefix(line, "\ufeff")
			first = false
		}
		if line == "" {
			done, err := dispatch()
			if err != nil || done {
				return err
			}
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		value := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
		if data.Len()+len(value)+1 > maxEventBytes {
			return errors.New("upstream SSE event exceeds size limit")
		}
		data.WriteString(value)
		data.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	_, err := dispatch()
	return err
}
