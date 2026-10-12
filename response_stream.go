package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// Frames are forwarded unchanged except for response metadata and omitted
// terminal output. Deltas are never rebuilt. No goroutine or pipe is needed.
type responseStreamReader struct {
	scanner  *bufio.Scanner
	metadata json.RawMessage
	output   *responseOutput
	pending  []byte
}

func newResponseStreamReader(reader io.Reader, metadata json.RawMessage) *responseStreamReader {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), maxEventBytes)
	scanner.Split(splitSSEFrame)
	return &responseStreamReader{
		scanner: scanner, metadata: metadata, output: newResponseOutput(), pending: []byte{},
	}
}

func (r *responseStreamReader) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	for len(r.pending) == 0 {
		if !r.scanner.Scan() {
			if err := r.scanner.Err(); err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		frame, err := r.frame(r.scanner.Bytes())
		if err != nil {
			return 0, err
		}
		r.pending = frame
	}
	n := copy(buffer, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func splitSSEFrame(data []byte, atEOF bool) (int, []byte, error) {
	var start int
	for start < len(data) {
		index := bytes.IndexByte(data[start:], '\n')
		if index < 0 {
			break
		}
		end := start + index
		if len(bytes.TrimSuffix(data[start:end], []byte("\r"))) == 0 {
			return end + 1, data[:end+1], nil
		}
		start = end + 1
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func (r *responseStreamReader) frame(frame []byte) ([]byte, error) {
	lines := bytes.Split(frame, []byte("\n"))
	payload := []byte{}
	for _, line := range lines {
		line = bytes.TrimSuffix(line, []byte("\r"))
		line = bytes.TrimPrefix(line, []byte("\xef\xbb\xbf"))
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		value := bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" "))
		payload = append(payload, value...)
		payload = append(payload, '\n')
	}
	payload = bytes.TrimSuffix(payload, []byte("\n"))
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return bytes.Clone(frame), nil
	}
	if len(r.metadata) == 0 && !bytes.Contains(payload, []byte(`"response"`)) &&
		!bytes.Contains(payload, []byte(`"response.output_item.done"`)) {
		return bytes.Clone(frame), nil
	}
	event := map[string]json.RawMessage{}
	if err := json.Unmarshal(payload, &event); err != nil || event == nil {
		return nil, errors.New("invalid JSON in upstream event stream")
	}
	response := event["response"]
	var err error
	switch jsonString(event["type"]) {
	case "response.output_item.done":
		var index int
		if raw := event["output_index"]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &index); err != nil {
				return nil, errors.New("invalid upstream output index")
			}
		}
		if err := r.output.add(index, event["item"]); err != nil {
			return nil, err
		}
	case "response.completed", "response.done", "response.incomplete", "response.failed":
		response, err = r.output.complete(response)
		if err != nil {
			return nil, err
		}
	}
	if len(response) == 0 || bytes.Equal(response, []byte("null")) {
		return bytes.Clone(frame), nil
	}
	if len(r.metadata) > 0 {
		response, err = responseMetadata(response, r.metadata)
		if err != nil {
			return nil, err
		}
	}
	if bytes.Equal(response, event["response"]) {
		return bytes.Clone(frame), nil
	}
	event["response"] = response
	encoded, err := json.Marshal(event)
	if err != nil {
		return nil, errors.New("cannot encode upstream response event")
	}
	if len(encoded) > maxEventBytes {
		return nil, errors.New("upstream SSE event exceeds size limit")
	}
	// Retain event/id/retry fields, comments, and CRLF framing. Replace only the
	// data field(s); SSE permits multiline JSON to become a single data line.
	result := []byte{}
	written := false
	for index, line := range lines {
		if bytes.HasPrefix(bytes.TrimPrefix(line, []byte("\xef\xbb\xbf")), []byte("data:")) {
			if written {
				continue
			}
			result = append(result, "data: "...)
			result = append(result, encoded...)
			if bytes.HasSuffix(line, []byte("\r")) {
				result = append(result, '\r')
			}
			written = true
		} else {
			result = append(result, line...)
		}
		if index < len(lines)-1 {
			result = append(result, '\n')
		}
	}
	return result, nil
}

func responseMetadata(raw []byte, metadata json.RawMessage) ([]byte, error) {
	response := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &response); err != nil || response == nil {
		return nil, errors.New("invalid upstream response for metadata")
	}
	response["metadata"] = metadata
	body, err := json.Marshal(response)
	if err != nil {
		return nil, errors.New("cannot encode upstream response metadata")
	}
	if len(body) > maxJSONBytes {
		return nil, errors.New("upstream response with metadata exceeds size limit")
	}
	return body, nil
}
