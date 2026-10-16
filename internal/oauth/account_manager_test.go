package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewAccountManager(t *testing.T) {
	var savedTokens Tokens
	saveCount := 0

	creds := Credentials{
		AccessToken:  "initial-access",
		RefreshToken: "initial-refresh",
		AccountID:    "acc-test-id",
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":  "new-access-token",
			"refresh_token": "new-refresh-token",
			"id_token":      "new-id-token",
		})
	}))
	defer server.Close()

	mgr, err := NewAccountManager(creds, server.Client(), server.URL, func(toks Tokens) error {
		savedTokens = toks
		saveCount++
		return nil
	})
	if err != nil {
		t.Fatalf("NewAccountManager failed: %v", err)
	}

	prof, err := mgr.Profile()
	if err != nil {
		t.Fatalf("mgr.Profile() failed: %v", err)
	}
	if prof.AccountID != "acc-test-id" {
		t.Fatalf("profile AccountID = %q, want acc-test-id", prof.AccountID)
	}

	// Token refresh
	updated, err := mgr.refresh(mgr.cachedDoc)
	if err != nil {
		t.Fatalf("mgr.refresh() failed: %v", err)
	}
	if updated.AccessToken != "new-access-token" {
		t.Fatalf("updated AccessToken = %q, want new-access-token", updated.AccessToken)
	}
	if saveCount != 1 {
		t.Fatalf("expected saveCount=1, got %d", saveCount)
	}
	if savedTokens.RefreshToken != "new-refresh-token" {
		t.Fatalf("savedTokens RefreshToken = %q, want new-refresh-token", savedTokens.RefreshToken)
	}
}

func TestSessionOptionsAndKeyIssuance(t *testing.T) {
	fakeToken := loginJWT("chatgpt-acc-1")
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":  fakeToken,
			"refresh_token": "dummy-refresh",
			"id_token":      fakeToken,
		})
	}))
	defer tokenServer.Close()

	ctx := context.Background()
	mgr, err := NewSessionManager(ctx, LoginOptions{
		Client:       tokenServer.Client(),
		TokenURL:     tokenServer.URL,
		AuthorizeURL: "https://auth.openai.com/oauth/authorize",
		ActivateWithOptions: func(ctx context.Context, opts SessionOptions, tokens Tokens) (Profile, AccountConfigResult, error) {
			return Profile{
				CredentialConfigured: true,
				AccountID:            "chatgpt-acc-1",
				Email:                "user@example.com",
				Name:                 "Test User",
			}, AccountConfigResult{
				AccountID: opts.AccountID,
				KeyID:     "key_test",
				KeySecret: "cxp_secret123",
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}
	defer mgr.Close()

	info, err := mgr.BeginWithOptions(ctx, SessionOptions{
		AccountID: "custom-acc",
		IssueKey:  "bifrost",
	})
	if err != nil {
		t.Fatalf("BeginWithOptions failed: %v", err)
	}

	status, ok := mgr.GetSession(info.ID)
	if !ok || status.Status != "pending" {
		t.Fatalf("GetSession failed: got %+v, %v", status, ok)
	}

	// Complete session
	prof, res, err := mgr.CompleteWithSessionID(ctx, info.ID, "auth-code", mgr.active.state)
	if err != nil {
		t.Fatalf("CompleteWithSessionID failed: %v", err)
	}
	if prof.AccountID != "chatgpt-acc-1" {
		t.Fatalf("profile AccountID = %q, want chatgpt-acc-1", prof.AccountID)
	}
	if res.KeySecret != "cxp_secret123" {
		t.Fatalf("result KeySecret = %q, want cxp_secret123", res.KeySecret)
	}

	// Verify status after complete
	status, ok = mgr.GetSession(info.ID)
	if !ok || status.Status != "configured" {
		t.Fatalf("status not configured: %+v", status)
	}
	if status.Key == nil || status.Key.Secret != "cxp_secret123" {
		t.Fatalf("session key not present: %+v", status.Key)
	}

	// Consume key
	mgr.ConsumeSessionKey(info.ID)
	status, _ = mgr.GetSession(info.ID)
	if status.Key != nil {
		t.Fatalf("session key was not cleared after ConsumeSessionKey")
	}
}
