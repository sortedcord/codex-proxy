package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// A native terminal event ends the stream immediately, after its rewritten bytes
// have been delivered. No network read (or optional legacy [DONE]) is required.
type responseStreamReader struct {
	scanner  *sseScanner
	metadata json.RawMessage
	output   *responseOutput
	pending  []byte
	done     bool
}

func newResponseStreamReader(reader io.Reader, metadata json.RawMessage) *responseStreamReader {
	return &responseStreamReader{
		scanner:  newSSEScanner(reader),
		metadata: metadata,
		output:   newResponseOutput(),
		pending:  []byte{},
	}
}

func (r *responseStreamReader) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	for len(r.pending) == 0 {
		if r.done {
			return 0, io.EOF
		}
		frame, err := r.scanner.next()
		if errors.Is(err, io.EOF) {
			return 0, io.ErrUnexpectedEOF
		}
		if err != nil {
			return 0, err
		}
		r.pending, err = r.frame(frame)
		if err != nil {
			return 0, err
		}
	}
	n := copy(buffer, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *responseStreamReader) frame(frame sseFrame) ([]byte, error) {
	if len(frame.data) == 0 {
		return bytes.Clone(frame.raw), nil
	}
	// [DONE] alone is not a native Responses terminal response.
	if bytes.Equal(frame.data, []byte("[DONE]")) {
		return nil, io.ErrUnexpectedEOF
	}
	event := map[string]json.RawMessage{}
	if err := json.Unmarshal(frame.data, &event); err != nil || event == nil {
		return nil, errors.New("invalid JSON in upstream event stream")
	}
	eventType := jsonString(event["type"])
	if eventType == "response.output_item.done" {
		var index int
		if raw := event["output_index"]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &index); err != nil {
				return nil, errors.New("invalid upstream output index")
			}
		}
		if err := r.output.add(index, event["item"]); err != nil {
			return nil, err
		}
	}
	terminal := isTerminalEventType(eventType)
	if eventType == "error" {
		r.done = true
		return bytes.Clone(frame.raw), nil
	}
	response := event["response"]
	var err error
	if terminal {
		response, err = r.output.complete(response)
		if err != nil {
			return nil, err
		}
	}
	if !isNull(response) && len(r.metadata) > 0 {
		response, err = responseMetadata(response, r.metadata)
		if err != nil {
			return nil, err
		}
	}
	if isNull(response) || bytes.Equal(response, event["response"]) {
		r.done = terminal
		return bytes.Clone(frame.raw), nil
	}
	event["response"] = response
	encoded, err := json.Marshal(event)
	if err != nil {
		return nil, errors.New("cannot encode upstream response event")
	}
	result := replaceSSEData(frame.raw, encoded)
	if len(result) > maxEventBytes {
		return nil, errors.New("upstream SSE frame exceeds size limit")
	}
	r.done = terminal
	return result, nil
}

// Retain event/id/retry fields, comments and CRLF; replace only data fields.
func replaceSSEData(frame, encoded []byte) []byte {
	lines := bytes.Split(frame, []byte("\n"))
	result := []byte{}
	written := false
	for index, line := range lines {
		if bytes.HasPrefix(bytes.TrimPrefix(line, []byte("\xef\xbb\xbf")), []byte("data:")) {
			if written {
				continue
			}
			if bytes.HasPrefix(line, []byte("\xef\xbb\xbf")) {
				result = append(result, "\xef\xbb\xbf"...)
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
	return result
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
