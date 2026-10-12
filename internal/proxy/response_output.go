package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
)

// Codex can omit completed items from a terminal response. Retain only complete
// native items, not deltas, with the same bound for both HTTP response modes.
type responseOutput struct {
	items map[int]json.RawMessage
	bytes int
}

func newResponseOutput() *responseOutput {
	return &responseOutput{items: map[int]json.RawMessage{}}
}

func (o *responseOutput) add(index int, item json.RawMessage) error {
	if len(item) == 0 || bytes.Equal(item, []byte("null")) {
		return errors.New("upstream output event missing item")
	}
	if index < 0 {
		return errors.New("upstream output index must be non-negative")
	}
	outputBytes := o.bytes + len(item) - len(o.items[index])
	if outputBytes > maxJSONBytes {
		return errors.New("upstream output exceeds size limit")
	}
	o.bytes = outputBytes
	o.items[index] = bytes.Clone(item)
	return nil
}

func (o *responseOutput) complete(raw []byte) ([]byte, error) {
	response := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &response); err != nil || response == nil {
		return nil, errors.New("upstream terminal event missing response")
	}
	output := []json.RawMessage{}
	if encoded, ok := response["output"]; ok {
		if err := json.Unmarshal(encoded, &output); err != nil {
			return nil, errors.New("invalid upstream response output")
		}
	}
	if len(output) > 0 || (len(o.items) == 0 && bytes.Equal(response["output"], []byte("[]"))) {
		return bytes.Clone(raw), nil
	}
	indexes := make([]int, 0, len(o.items))
	for index := range o.items {
		indexes = append(indexes, index)
	}
	slices.Sort(indexes)
	output = make([]json.RawMessage, 0, len(indexes))
	for _, index := range indexes {
		output = append(output, o.items[index])
	}
	setJSON(response, "output", output)
	result, err := json.Marshal(response)
	if err != nil {
		return nil, errors.New("cannot encode upstream response output")
	}
	if len(result) > maxJSONBytes {
		return nil, errors.New("upstream response exceeds size limit")
	}
	return result, nil
}
