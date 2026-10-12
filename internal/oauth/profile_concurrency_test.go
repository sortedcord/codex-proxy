package oauth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestCachedProfileDoesNotWaitForRefresh(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, `{"access_token":"new-access"}`)
	}))
	defer upstream.Close()
	defer once.Do(func() { close(release) })
	manager := newTestAuth(t, upstream.URL)
	snapshot, err := manager.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	refreshed := make(chan error, 1)
	go func() {
		_, _, err := manager.Recover(context.Background(), snapshot)
		refreshed <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("refresh did not begin")
	}
	profileReady := make(chan error, 1)
	go func() { _, err := manager.Profile(); profileReady <- err }()
	select {
	case err := <-profileReady:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cached management profile waited for the refresh network call")
	}
	once.Do(func() { close(release) })
	if err := <-refreshed; err != nil {
		t.Fatal(err)
	}
}
