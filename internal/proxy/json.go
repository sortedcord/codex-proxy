package proxy

import (
	"encoding/json"
	"io"

	"github.com/sortedcord/codex-proxy/internal/httputil"
)

const (
	maxRequestBytes = httputil.MaxRequestBytes
	maxEventBytes   = httputil.MaxEventBytes
	maxJSONBytes    = httputil.MaxJSONBytes
)

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	return httputil.ReadBounded(reader, limit)
}

func jsonString(raw json.RawMessage) string {
	return httputil.JSONString(raw)
}

func setJSON(object map[string]json.RawMessage, key string, value any) {
	httputil.SetJSON(object, key, value)
}
