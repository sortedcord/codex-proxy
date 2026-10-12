package httputil

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadBounded(t *testing.T) {
	data := []byte("hello world")
	got, err := ReadBounded(bytes.NewReader(data), 20)
	if err != nil {
		t.Fatalf("bounded read failed: %v", err)
	}
	if string(got) != "hello world" {
		t.Fatalf("bounded read=%q, want hello world", got)
	}

	_, err = ReadBounded(bytes.NewReader(data), 5)
	if err == nil {
		t.Fatal("expected error on oversized input")
	}
}

func TestJSONHelpers(t *testing.T) {
	msg := json.RawMessage(`"test-value"`)
	if got := JSONString(msg); got != "test-value" {
		t.Fatalf("JSONString = %q, want %q", got, "test-value")
	}

	obj := map[string]json.RawMessage{}
	SetJSON(obj, "key", 123)
	if string(obj["key"]) != "123" {
		t.Fatalf("SetJSON value = %s, want 123", obj["key"])
	}
}

func TestBearerMatches(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if BearerMatches(req, "") {
		t.Fatal("empty expected key must fail closed")
	}

	req.Header.Set("Authorization", "Bearer secret-token")
	if !BearerMatches(req, "secret-token") {
		t.Fatal("matching token should succeed")
	}

	if BearerMatches(req, "wrong-token") {
		t.Fatal("mismatched token should fail")
	}

	req.Header.Set("Authorization", "Basic secret-token")
	if BearerMatches(req, "secret-token") {
		t.Fatal("non-bearer token should fail")
	}
}

func TestRejectUnreadBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("body"))
	req.ProtoMajor = 1
	rec := httptest.NewRecorder()
	RejectUnreadBody(rec, req)
	if rec.Header().Get("Connection") != "close" {
		t.Fatal("expected Connection: close header")
	}
}

func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, http.StatusOK, []byte(`{"ok":true}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Fatal("missing Content-Type: application/json")
	}
	if rec.Body.String() != `{"ok":true}` {
		t.Fatalf("body = %s, want %s", rec.Body.String(), `{"ok":true}`)
	}
}
