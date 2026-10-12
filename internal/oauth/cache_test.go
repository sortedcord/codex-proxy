package oauth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestCacheDetectsSameSizePreservedTimestampRotation(t *testing.T) {
	manager := newTestAuth(t, "http://127.0.0.1:1")
	writeTestAuth(t, manager.path, "access-AAAA", "refresh-1")
	info, err := os.Stat(manager.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Current(context.Background()); err != nil {
		t.Fatal(err)
	}
	writeTestAuth(t, manager.path, "access-BBBB", "refresh-2")
	if err := os.Chtimes(manager.path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	creds, err := manager.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if creds.AccessToken != "access-BBBB" {
		t.Fatal("cache missed external same-size token rotation")
	}
}

func TestFailedRefreshDoesNotMutateCachedDocument(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"new-access","id_token":"new-id"}`)
	}))
	defer upstream.Close()
	manager := newTestAuth(t, upstream.URL)
	snapshot, err := manager.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	manager.persist = func(string, []byte) error { return errors.New("save failed") }
	if _, _, err := manager.Recover(context.Background(), snapshot); err == nil {
		t.Fatal("expected save failure")
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if got := jsonString(manager.cachedDoc.tokens["access_token"]); got != snapshot.AccessToken {
		t.Fatal("failed persistence mutated cached token")
	}
	if got := jsonString(manager.cachedDoc.tokens["id_token"]); got != "dummy-id" {
		t.Fatal("failed persistence mutated cached identity")
	}
}

func TestFreshSnapshotCannotAdoptReplacementAccount(t *testing.T) {
	manager := newTestAuth(t, "http://127.0.0.1:1")
	snapshot, err := manager.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	snapshot.AccessToken = testJWT(snapshot.LastRefresh.Add(-1))
	snapshot.expiresAt = tokenExpiry(snapshot.AccessToken)
	if _, err := manager.Replace(context.Background(), Tokens{
		AccessToken: loginJWT("replacement"), RefreshToken: "dummy",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Fresh(context.Background(), snapshot); err == nil {
		t.Fatal("proactive refresh crossed login generations")
	}
}
