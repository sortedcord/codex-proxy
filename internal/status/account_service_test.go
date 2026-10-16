package status

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sortedcord/codex-proxy/internal/quota"
)

func TestAccountServiceLifecycleAndHTTP(t *testing.T) {
	now := time.Now().UTC()
	fetchCalls := 0

	svc, err := NewAccountService(context.Background(), AccountOptions{
		Namespace: "test-account-1",
		Interval:  100 * time.Millisecond,
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

func TestAccountServiceSanitizesFetchErrorAndEnforcesTimeout(t *testing.T) {
	var timeoutObserved bool
	fetch := func(ctx context.Context) (quota.Snapshot, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 31*time.Second {
			timeoutObserved = false
		} else {
			timeoutObserved = true
		}
		return quota.Snapshot{}, errors.New("private upstream database secret connection refused")
	}

	svc, err := NewAccountService(context.Background(), AccountOptions{
		Namespace: "test-err",
		Interval:  time.Hour,
		Fetch:     fetch,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		_, _, info := svc.Latest()
		if info.LastError != "" {
			if strings.Contains(info.LastError, "private upstream database") {
				t.Fatalf("private error details leaked: %s", info.LastError)
			}
			if info.LastError != "quota observation failed; upstream unavailable" {
				t.Fatalf("unexpected sanitized error: %s", info.LastError)
			}
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if !timeoutObserved {
		t.Fatal("per-poll timeout was not applied to fetch context")
	}
}
