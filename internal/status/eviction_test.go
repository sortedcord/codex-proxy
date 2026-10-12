package status

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sortedcord/codex-proxy/internal/oauth"
	"github.com/sortedcord/codex-proxy/internal/quota"
)

func TestEvictionFailsClosedWhenAccountUnavailable(t *testing.T) {
	var failing atomic.Bool
	service, err := NewService(context.Background(), Options{
		Dir: filepath.Join(t.TempDir(), "history"), MaxSamples: 5, Interval: time.Hour,
		Account: func() (oauth.Profile, error) {
			if failing.Load() {
				return oauth.Profile{}, errors.New("transient account error")
			}
			return oauth.Profile{AccountID: "a"}, nil
		},
		Fetch: func(context.Context) (quota.Snapshot, error) {
			return quota.Snapshot{}, errors.New("no observation")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err := service.Prepare("b"); err != nil {
		t.Fatal(err)
	}
	failing.Store(true)
	if err := service.Prepare("c"); err == nil {
		t.Fatal("unknown active account allowed cache eviction")
	}
	service.mu.Lock()
	_, keptA := service.histories["a"]
	_, keptB := service.histories["b"]
	count := len(service.histories)
	service.mu.Unlock()
	if !keptA {
		t.Fatal("active history was evicted")
	}
	if !keptB {
		t.Fatal("prior history was evicted on failed preparation")
	}
	if count != 2 {
		t.Fatalf("cached journals = %d, want 2", count)
	}
	failing.Store(false)
	if err := service.Prepare("c"); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.histories["a"] == nil {
		t.Fatal("recovered eviction displaced active history")
	}
	if service.histories["b"] != nil {
		t.Fatal("inactive history not evicted")
	}
}
