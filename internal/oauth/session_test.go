package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func loginJWT(account string) string {
	body, _ := json.Marshal(map[string]any{
		"exp": time.Now().Add(time.Hour).Unix(), "email": "user@example.invalid",
		"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": account},
	})
	return "header." + base64.RawURLEncoding.EncodeToString(body) + ".signature"
}

func sessionsForTest(
	t *testing.T,
	handler http.Handler,
	activate func(context.Context, Tokens) (Profile, error),
) *SessionManager {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	manager, err := NewSessionManager(context.Background(), LoginOptions{
		Client: newHTTPClient(), AuthorizeURL: server.URL + "/authorize", TokenURL: server.URL + "/token",
		CallbackAddresses: []string{"127.0.0.1:0"}, Activate: activate,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	return manager
}

func TestOAuthPKCEStateSingleUseAndManualCallback(t *testing.T) {
	var calls atomic.Int32
	var challenge string
	manager := sessionsForTest(t, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		_ = request.ParseForm()
		digest := sha256.Sum256([]byte(request.Form.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(digest[:]) != challenge {
			t.Error("PKCE challenge did not match verifier")
		}
		if request.Form.Get("grant_type") != "authorization_code" {
			t.Errorf("grant type=%q, want authorization_code", request.Form.Get("grant_type"))
		}
		if request.Form.Get("client_id") != ClientID {
			t.Errorf("client ID=%q, want %q", request.Form.Get("client_id"), ClientID)
		}
		_ = json.NewEncoder(w).Encode(Tokens{
			AccessToken: loginJWT("account-new"), IDToken: loginJWT("account-new"), RefreshToken: "dummy-refresh",
		})
	}), func(ctx context.Context, tokens Tokens) (Profile, error) { return tokens.Profile() })
	info, err := manager.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(info.LoginURL)
	challenge = parsed.Query().Get("code_challenge")
	if method := parsed.Query().Get("code_challenge_method"); method != "S256" {
		t.Fatalf("PKCE method=%q, want S256", method)
	}
	if size := len(parsed.Query().Get("state")); size != 43 {
		t.Fatalf("OAuth state length=%d, want 43", size)
	}
	reused, _ := manager.Begin(context.Background())
	if reused.LoginURL != info.LoginURL {
		t.Fatal("pending session was not reused")
	}
	if _, err := manager.Complete(context.Background(), "code", "wrong-state"); !errors.Is(err, ErrInvalidState) {
		t.Fatal("wrong state accepted")
	}
	redirect := parsed.Query().Get("redirect_uri")
	foreignURL := "https://attacker.invalid/auth/callback?code=x&state=x"
	if _, err := manager.CompleteURL(context.Background(), foreignURL); err == nil {
		t.Fatal("foreign callback URL accepted")
	}
	callback := redirect + "?code=dummy-code&state=" + parsed.Query().Get("state")
	identity, err := manager.CompleteURL(context.Background(), callback)
	if err != nil {
		t.Fatalf("manual callback failed: %v", err)
	}
	if identity.AccountID != "account-new" {
		t.Fatalf("callback account=%q, want account-new", identity.AccountID)
	}
	_, replayError := manager.Complete(context.Background(), "code", parsed.Query().Get("state"))
	if !errors.Is(replayError, ErrSessionUsed) {
		t.Fatal("consumed code replay accepted")
	}
	if calls.Load() != 1 {
		t.Fatal("token exchange was replayed")
	}
	listener, err := net.Listen("tcp4", strings.TrimPrefix(strings.TrimSuffix(redirect, "/auth/callback"), "http://"))
	if err != nil {
		t.Fatal("callback listener was not released", err)
	}
	_ = listener.Close()
}

func TestOAuthBrowserCallbackAndExpiry(t *testing.T) {
	manager := sessionsForTest(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(Tokens{AccessToken: loginJWT("account-new"), RefreshToken: "dummy-refresh"})
	}), func(ctx context.Context, tokens Tokens) (Profile, error) { return tokens.Profile() })
	info, _ := manager.Begin(context.Background())
	parsed, _ := url.Parse(info.LoginURL)
	callback := parsed.Query().Get("redirect_uri") + "?code=test&state=" + parsed.Query().Get("state")
	resp, err := http.Get(callback)
	if err != nil {
		t.Fatal("browser callback connection was closed before response", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal("browser callback failed")
	}
	manager.options.Lifetime = 10 * time.Millisecond
	info, _ = manager.Begin(context.Background())
	parsed, _ = url.Parse(info.LoginURL)
	time.Sleep(30 * time.Millisecond)
	_, expiredError := manager.Complete(context.Background(), "test", parsed.Query().Get("state"))
	if !errors.Is(expiredError, ErrSessionExpired) {
		t.Fatal("expired session accepted")
	}
}

func TestLoginReplacementIsAccountBoundAndFailureSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	manager, err := NewManager(path, newHTTPClient(), DefaultTokenURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Profile(); !errors.Is(err, ErrUnavailable) {
		t.Fatal("missing credentials were treated as configured")
	}
	_, err = manager.Replace(context.Background(), Tokens{AccessToken: loginJWT("account-a"), RefreshToken: "dummy-a"})
	if err != nil {
		t.Fatal(err)
	}
	old, _ := manager.Current(context.Background())
	_, err = manager.Replace(context.Background(), Tokens{AccessToken: loginJWT("account-b"), RefreshToken: "dummy-b"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.Recover(context.Background(), old); err == nil {
		t.Fatal("old request adopted new-account credentials")
	}
	before, _ := os.ReadFile(path)
	invalidTokens := Tokens{AccessToken: "invalid-no-account", RefreshToken: "dummy"}
	if _, err := manager.Replace(context.Background(), invalidTokens); err == nil {
		t.Fatal("invalid replacement accepted")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("failed replacement damaged saved credentials")
	}
	manager.persist = func(string, []byte) error { return errors.New("simulated persistence failure") }
	candidate := Tokens{AccessToken: loginJWT("account-c"), RefreshToken: "dummy-c"}
	if _, err := manager.Replace(context.Background(), candidate); err == nil {
		t.Fatal("failed credential save was accepted")
	}
	identity, err := manager.Profile()
	if err != nil {
		t.Fatalf("active profile unavailable after failed save: %v", err)
	}
	if identity.AccountID != "account-b" {
		t.Fatalf("active account=%q after failed save, want account-b", identity.AccountID)
	}
}
