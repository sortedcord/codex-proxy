// Package httputil provides shared HTTP and JSON utilities for codex-proxy.
package httputil

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/terastal-hq/proxylib/httpx"
)

const (
	MaxRequestBytes = 16 << 20
	MaxEventBytes   = 16 << 20
	MaxJSONBytes    = 32 << 20
	WriteDeadline   = 30 * time.Second
)

// ReadBounded reads from reader up to limit bytes, returning an error if limit is exceeded.
func ReadBounded(reader io.Reader, limit int64) ([]byte, error) {
	return httpx.ReadBounded(reader, limit)
}

// JSONString unmarshals a JSON raw message into a string, returning empty string on failure.
func JSONString(raw json.RawMessage) string {
	return httpx.JSONString(raw)
}

// SetJSON encodes value into raw JSON and stores it in object under key.
func SetJSON(object map[string]json.RawMessage, key string, value any) {
	httpx.SetJSON(object, key, value)
}

// BearerMatches checks in constant time whether request's Authorization Bearer token matches key.
func BearerMatches(r *http.Request, key string) bool {
	return httpx.BearerMatches(r, key)
}

// RejectUnreadBody prevents the server from blocking while draining unread request bodies.
func RejectUnreadBody(w http.ResponseWriter, r *http.Request) {
	if r.ProtoMajor == 1 && r.Body != nil && r.Body != http.NoBody {
		w.Header().Set("Connection", "close")
		_ = http.NewResponseController(w).SetReadDeadline(time.Now())
	}
}

// WriteJSON sets content-type, write deadline, writes the HTTP status and body.
func WriteJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(WriteDeadline))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
