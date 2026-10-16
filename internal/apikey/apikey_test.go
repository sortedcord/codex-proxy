package apikey

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGenerateAndHash(t *testing.T) {
	key, err := Generate()
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	if !strings.HasPrefix(key, Prefix) {
		t.Fatalf("expected key to start with %q, got %q", Prefix, key)
	}
	// Prefix (4 chars) + 43 base62 chars = 47 chars
	if len(key) != 47 {
		t.Fatalf("expected key length 47, got %d", len(key))
	}

	h := Hash(key)
	if !strings.HasPrefix(h, HashPrefix) {
		t.Fatalf("expected hash to start with %q, got %q", HashPrefix, h)
	}

	if !Match(key, h) {
		t.Fatalf("Match failed for generated key and its hash")
	}
	if Match("wrong_key", h) {
		t.Fatalf("Match succeeded for incorrect key")
	}

	// Plaintext match fallback
	if !Match("my-plain-secret", "my-plain-secret") {
		t.Fatalf("Match failed for plaintext key")
	}
	if Match("my-plain-secret", "other-secret") {
		t.Fatalf("Match succeeded for mismatched plaintext key")
	}
}

func TestGenerateID(t *testing.T) {
	id, err := GenerateID()
	if err != nil {
		t.Fatalf("GenerateID failed: %v", err)
	}
	if !strings.HasPrefix(id, "key_") {
		t.Fatalf("expected id to start with 'key_', got %q", id)
	}
	if len(id) != 12 { // "key_" (4) + 8 hex chars = 12
		t.Fatalf("expected id length 12, got %d", len(id))
	}
}

func TestLast4(t *testing.T) {
	if got := Last4("cxp_1234567890abcdef"); got != "cdef" {
		t.Fatalf("expected 'cdef', got %q", got)
	}
	if got := Last4("abc"); got != "abc" {
		t.Fatalf("expected 'abc', got %q", got)
	}
}

func TestExtract(t *testing.T) {
	tests := []struct {
		name       string
		headers    map[string]string
		rawURL     string
		allowQuery bool
		wantKey    string
	}{
		{
			name:       "x-api-key",
			headers:    map[string]string{"x-api-key": "secret1"},
			allowQuery: true,
			wantKey:    "secret1",
		},
		{
			name:       "api-key",
			headers:    map[string]string{"api-key": "secret2"},
			allowQuery: true,
			wantKey:    "secret2",
		},
		{
			name:       "x-goog-api-key",
			headers:    map[string]string{"x-goog-api-key": "secret3"},
			allowQuery: true,
			wantKey:    "secret3",
		},
		{
			name:       "Authorization Bearer",
			headers:    map[string]string{"Authorization": "Bearer secret4"},
			allowQuery: true,
			wantKey:    "secret4",
		},
		{
			name:       "Query param allowed",
			rawURL:     "/models?key=secret5",
			allowQuery: true,
			wantKey:    "secret5",
		},
		{
			name:       "Query param rejected when allowQuery is false",
			rawURL:     "/admin/accounts?key=secret5",
			allowQuery: false,
			wantKey:    "",
		},
		{
			name:       "Prioritize header over query",
			headers:    map[string]string{"x-api-key": "header-secret"},
			rawURL:     "/models?key=query-secret",
			allowQuery: true,
			wantKey:    "header-secret",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := tt.rawURL
			if target == "" {
				target = "/test"
			}
			req := httptest.NewRequest(http.MethodGet, target, nil)
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			got := Extract(req, tt.allowQuery)
			if got != tt.wantKey {
				t.Fatalf("Extract() = %q, want %q", got, tt.wantKey)
			}
		})
	}

	if Extract(nil, true) != "" {
		t.Fatalf("Extract(nil) should be empty")
	}
}

func TestRegistry(t *testing.T) {
	key1, _ := Generate()
	key2, _ := Generate()

	bindings := map[string]Binding{
		Hash(key1): {AccountID: "acc1", KeyID: "k1"},
		Hash(key2): {AccountID: "acc2", KeyID: "k2"},
	}

	reg, err := NewRegistry(bindings)
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}

	if reg.Size() != 2 {
		t.Fatalf("expected size 2, got %d", reg.Size())
	}
	if !reg.HasKeyID("k1") || !reg.HasKeyID("k2") {
		t.Fatalf("HasKeyID failed")
	}
	if reg.HasKeyID("k3") {
		t.Fatalf("HasKeyID succeeded for nonexistent key")
	}

	b, ok := reg.Lookup(key1)
	if !ok || b.AccountID != "acc1" || b.KeyID != "k1" {
		t.Fatalf("Lookup key1 failed: got %+v, %v", b, ok)
	}

	b, ok = reg.Lookup(key2)
	if !ok || b.AccountID != "acc2" || b.KeyID != "k2" {
		t.Fatalf("Lookup key2 failed: got %+v, %v", b, ok)
	}

	_, ok = reg.Lookup("invalid_key")
	if ok {
		t.Fatalf("Lookup invalid_key should fail")
	}

	// Invalid hash prefix in NewRegistry
	_, err = NewRegistry(map[string]Binding{"invalid_hash": {AccountID: "acc"}})
	if err == nil {
		t.Fatalf("expected error for invalid hash prefix")
	}

	// Invalid hex in NewRegistry
	_, err = NewRegistry(map[string]Binding{"sha256:nothex": {AccountID: "acc"}})
	if err == nil {
		t.Fatalf("expected error for invalid hex in hash")
	}
}
