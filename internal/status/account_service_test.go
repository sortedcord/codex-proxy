package status

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/sortedcord/codex-proxy/internal/quota"
)

func TestAccountServiceLifecycleAndHTTP(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "history")
	now := time.Now().UTC()
	fetchCalls := 0

	svc, err := NewAccountService(context.Background(), AccountOptions{
		Dir:        dir,
		Namespace:  "test-account-1",
		MaxSamples: 100,
		Interval:   100 * time.Millisecond,
		Fetch: func(ctx context.Context) (quota.Snapshot, error) {
			fetchCalls++
			return quota.Snapshot{
				AccountID:  "test-account-1",
				ObservedAt: now,
				Source:     "wham/usage",
				Pools: map[string]quota.Limit{
					"codex": {ID: "codex", DisplayName: "Codex"},
				},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewAccountService failed: %v", err)
	}
	defer svc.Close()

	// Wait for initial poll
	time.Sleep(50 * time.Millisecond)

	snap, avail, polling := svc.Latest()
	if !avail {
		t.Fatalf("expected snapshot available")
	}
	if snap.AccountID != "test-account-1" {
		t.Fatalf("snapshot accountID = %q, want test-account-1", snap.AccountID)
	}
	if polling.LastError != "" {
		t.Fatalf("polling had unexpected error: %s", polling.LastError)
	}

	// Test GET /status/limit
	req := httptest.NewRequest(http.MethodGet, "/status/limit", nil)
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /status/limit returned %d, want 200", w.Code)
	}

	// Test GET /status/usage
	req = httptest.NewRequest(http.MethodGet, "/status/usage", nil)
	w = httptest.NewRecorder()
	svc.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /status/usage returned %d, want 200", w.Code)
	}

	// Test wrong method
	req = httptest.NewRequest(http.MethodPost, "/status/limit", nil)
	w = httptest.NewRecorder()
	svc.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /status/limit returned %d, want 405", w.Code)
	}
}
