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

func TestServicePollingStalenessAndAccountIsolation(t *testing.T) {
	var active atomic.Value
	active.Store("account-a")
	var failed atomic.Bool
	service, err := NewService(context.Background(), Options{
		Dir: filepath.Join(t.TempDir(), "history"), MaxSamples: 5, Interval: time.Hour,
		Account: func() (oauth.Profile, error) {
			return oauth.Profile{CredentialConfigured: true, AccountID: active.Load().(string)}, nil
		},
		Fetch: func(ctx context.Context) (quota.Snapshot, error) {
			if failed.Load() {
				return quota.Snapshot{}, errors.New("private failure details")
			}
			return testSample(active.Load().(string), time.Now().UTC()), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	wait := func(account string, wantError bool) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			_, available, info := service.Latest(account)
			if available && (info.LastError != "") == wantError {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("status polling did not settle")
	}
	wait("account-a", false)
	failed.Store(true)
	service.TriggerPoll()
	wait("account-a", true)
	sample, _, info := service.Latest("account-a")
	if !service.Stale(sample, info) {
		t.Fatal("failed quota observation was not marked stale")
	}
	if info.LastError == "private failure details" {
		t.Fatal("polling status exposed private failure details")
	}
	failed.Store(false)
	if err := service.Prepare("account-b"); err != nil {
		t.Fatal(err)
	}
	active.Store("account-b")
	service.TriggerPoll()
	wait("account-b", false)
	result, err := service.Query("account-b", Query{Pool: "all", Window: "all", Order: "desc", Limit: 100})
	if err != nil {
		t.Fatalf("history query failed: %v", err)
	}
	if len(result.Samples) != 1 {
		t.Fatalf("active account sample count=%d, want 1", len(result.Samples))
	}
	if result.Samples[0].AccountID != "account-b" {
		t.Fatal("history crossed account boundaries")
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	if err := service.Prepare("account-c"); err == nil {
		t.Fatal("closed service reopened history")
	}
}
