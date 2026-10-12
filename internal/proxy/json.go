package proxy

import (
	"encoding/json"
	"errors"
	"io"
)

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errors.New("body exceeds size limit")
	}
	return body, nil
}

func jsonString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func setJSON(object map[string]json.RawMessage, key string, value any) {
	object[key], _ = json.Marshal(value)
}
