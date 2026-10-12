package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLoginConcurrentCompletionAndDisconnect(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	manager := sessionsForTest(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_ = json.NewEncoder(w).Encode(Tokens{AccessToken: loginJWT("account-new"), RefreshToken: "dummy-refresh"})
	}), func(ctx context.Context, tokens Tokens) (Profile, error) { return tokens.Profile() })
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	info, _ := manager.Begin(context.Background())
	state := manager.active.state
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := manager.Complete(ctx, "test", state); done <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("exchange did not start")
	}
	if _, err := manager.Begin(context.Background()); !errors.Is(err, ErrLoginInProgress) {
		t.Fatal("overlapping login began")
	}
	if _, err := manager.Complete(context.Background(), "test", state); !errors.Is(err, ErrLoginInProgress) {
		t.Fatal("concurrent code exchange was accepted")
	}
	cancel() // The submitting client has gone away; the one-time exchange still commits.
	releaseOnce.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal("successful exchange was abandoned after client cancellation", err)
	}
	if !strings.Contains(info.LoginURL, "code_challenge=") {
		t.Fatal("login URL lacked PKCE")
	}
}
