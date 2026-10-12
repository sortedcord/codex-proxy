// Package httputil provides shared HTTP and JSON utilities for codex-proxy.
package httputil

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	MaxRequestBytes = 16 << 20
	MaxEventBytes   = 16 << 20
	MaxJSONBytes    = 32 << 20
	WriteDeadline   = 30 * time.Second
)

// ReadBounded reads from reader up to limit bytes, returning an error if limit is exceeded.
func ReadBounded(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errors.New("body exceeds size limit")
	}
	return body, nil
}

// JSONString unmarshals a JSON raw message into a string, returning empty string on failure.
func JSONString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

// SetJSON encodes value into raw JSON and stores it in object under key.
func SetJSON(object map[string]json.RawMessage, key string, value any) {
	object[key], _ = json.Marshal(value)
}

// BearerMatches checks in constant time whether request's Authorization Bearer token matches key.
func BearerMatches(r *http.Request, key string) bool {
	if key == "" {
		return false
	}
	value := r.Header.Get("Authorization")
	parts := strings.SplitN(value, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return false
	}
	expected := sha256.Sum256([]byte(key))
	provided := sha256.Sum256([]byte(parts[1]))
	return subtle.ConstantTimeCompare(expected[:], provided[:]) == 1
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
