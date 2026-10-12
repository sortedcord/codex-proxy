package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testJWT(expiry time.Time) string {
	body, _ := json.Marshal(map[string]any{"exp": expiry.Unix()})
	return "header." + base64.RawURLEncoding.EncodeToString(body) + ".signature"
}

func writeTestAuth(t *testing.T, path, access, refresh string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"auth_mode": "chatgpt", "OPENAI_API_KEY": nil,
		"last_refresh": time.Now().UTC().Format(time.RFC3339),
		"extra":        map[string]string{"preserve": "yes"},
		"tokens": map[string]string{
			"access_token": access, "refresh_token": refresh,
			"id_token": "dummy-id", "account_id": "test-account", "extra_token": "preserve",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(path, body); err != nil {
		t.Fatal(err)
	}
}

func newTestAuth(t *testing.T, tokenURL string) *authManager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	writeTestAuth(
		t,
		path,
		testJWT(time.Now().Add(time.Hour)),
		"dummy-refresh",
	)
	client := newHTTPClient()
	t.Cleanup(client.CloseIdleConnections)
	auth, err := newAuthManager(path, client, tokenURL)
	if err != nil {
		t.Fatal(err)
	}
	return auth
}

func TestConcurrentRefreshPersistsRotation(t *testing.T) {
	var calls atomic.Int32
	newAccess := testJWT(time.Now().Add(2 * time.Hour))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		payload := map[string]string{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload["client_id"] != oauthClientID || payload["grant_type"] != "refresh_token" {
			t.Errorf("incorrect refresh request: %v", payload)
		}
		if payload["refresh_token"] != "dummy-refresh" {
			t.Error("incorrect refresh token")
		}
		time.Sleep(20 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token": newAccess, "refresh_token": "rotated-refresh", "id_token": "rotated-id",
		})
	}))
	defer upstream.Close()
	auth := newTestAuth(t, upstream.URL)
	writeTestAuth(
		t,
		auth.path,
		testJWT(time.Now().Add(time.Minute)),
		"dummy-refresh",
	)
	var group sync.WaitGroup
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			creds, err := auth.current(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			if creds.accessToken != newAccess || creds.refreshToken != "rotated-refresh" {
				t.Error("rotation not observed")
			}
		}()
	}
	group.Wait()
	if calls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want 1", calls.Load())
	}
	doc, err := readAuth(auth.path)
	if err != nil {
		t.Fatal(err)
	}
	if jsonString(doc.tokens["extra_token"]) != "preserve" || jsonString(doc.tokens["id_token"]) != "rotated-id" {
		t.Error("token fields were lost")
	}
	extra := map[string]string{}
	_ = json.Unmarshal(doc.root["extra"], &extra)
	if extra["preserve"] != "yes" {
		t.Error("unknown root metadata was lost")
	}
	info, err := os.Stat(auth.path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("auth permissions = %o", info.Mode().Perm())
	}
}

func TestReactiveRefreshCoalescesAndReloads(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"access_token":"new-access","refresh_token":"new-refresh"}`)
	}))
	defer upstream.Close()
	auth := newTestAuth(t, upstream.URL)
	old, err := auth.current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for range 10 {
		creds, _, err := auth.recover(context.Background(), old.accessToken)
		if err != nil || creds.accessToken != "new-access" {
			t.Fatalf("recover = %v, %v", creds.accessToken, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("refresh calls = %d", calls.Load())
	}
	writeTestAuth(
		t,
		auth.path,
		"external-access",
		"external-refresh",
	)
	creds, refreshed, err := auth.recover(context.Background(), "new-access")
	reloaded := err == nil && !refreshed && creds.accessToken == "external-access"
	if !reloaded {
		t.Fatalf("disk reload failed: refreshed=%v, error=%v", refreshed, err)
	}
}

func TestPermanentRefreshFailureCachedUntilCredentialsChange(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"refresh_token_reused","sensitive":"secret-must-not-leak"}`)
	}))
	defer upstream.Close()
	auth := newTestAuth(t, upstream.URL)
	writeTestAuth(
		t,
		auth.path,
		"rejected",
		"dummy-refresh",
	)
	for range 3 {
		_, _, err := auth.recover(context.Background(), "rejected")
		if err == nil || strings.Contains(err.Error(), "secret-must-not-leak") {
			t.Fatal("refresh errors must be sanitized")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("refresh calls = %d", calls.Load())
	}
	writeTestAuth(
		t,
		auth.path,
		"new",
		"fresh-login",
	)
	_, _, _ = auth.recover(context.Background(), "new")
	if calls.Load() != 2 {
		t.Fatal("new login did not reset failure cache")
	}
}

func TestRefreshDoesNotOverwriteExternalRotation(t *testing.T) {
	var auth *authManager
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeTestAuth(
			t,
			auth.path,
			"external-access",
			"external-refresh",
		)
		_, _ = io.WriteString(w, `{"access_token":"our-access","refresh_token":"our-refresh"}`)
	}))
	defer upstream.Close()
	auth = newTestAuth(t, upstream.URL)
	writeTestAuth(
		t,
		auth.path,
		"rejected",
		"dummy-refresh",
	)
	creds, _, err := auth.recover(context.Background(), "rejected")
	if err != nil || creds.accessToken != "external-access" {
		t.Fatalf("external rotation overwritten: %v", err)
	}
}

func TestRefreshCompletesAfterClientCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, `{"access_token":"new-access"}`)
	}))
	defer upstream.Close()
	auth := newTestAuth(t, upstream.URL)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	writeTestAuth(
		t,
		auth.path,
		"rejected",
		"dummy-refresh",
	)
	go func() {
		_, _, err := auth.recover(ctx, "rejected")
		done <- err
	}()
	<-started
	cancel()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	doc, err := readAuth(auth.path)
	persisted := err == nil && doc.creds.accessToken == "new-access"
	preservedRefresh := doc.creds.refreshToken == "dummy-refresh"
	if !persisted || !preservedRefresh {
		t.Fatal("refresh did not persist after cancellation or lost omitted refresh token")
	}
}

func TestAuthFailsClosedOnLogoutAndAccountSwitch(t *testing.T) {
	auth := newTestAuth(t, "http://127.0.0.1:1")
	doc, err := readAuth(auth.path)
	if err != nil {
		t.Fatal(err)
	}
	setJSON(doc.tokens, "account_id", "another-account")
	setJSON(doc.root, "tokens", doc.tokens)
	body, _ := json.Marshal(doc.root)
	if err := atomicWrite(auth.path, body); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.current(context.Background()); err == nil {
		t.Fatal("account switch accepted")
	}
	if err := os.Remove(auth.path); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.current(context.Background()); err == nil {
		t.Fatal("deleted credentials accepted")
	}
}

func TestRefreshScheduling(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name  string
		creds credentials
		want  bool
	}{
		{name: "fresh JWT", creds: credentials{accessToken: testJWT(now.Add(time.Hour))}},
		{name: "near expiry", creds: credentials{accessToken: testJWT(now.Add(time.Minute))}, want: true},
		{name: "expired", creds: credentials{accessToken: testJWT(now.Add(-time.Minute))}, want: true},
		{name: "opaque recent", creds: credentials{accessToken: "opaque", lastRefresh: now}},
		{
			name:  "opaque old",
			creds: credentials{accessToken: "opaque", lastRefresh: now.Add(-9 * 24 * time.Hour)},
			want:  true,
		},
		{name: "opaque unknown age", creds: credentials{accessToken: "opaque"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := needsRefresh(test.creds, now); got != test.want {
				t.Errorf("needsRefresh = %v, want %v", got, test.want)
			}
		})
	}
}
