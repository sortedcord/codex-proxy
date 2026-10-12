package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/sortedcord/codex-proxy/internal/storage"
)

func workspaceJWT(user string) string {
	body, _ := json.Marshal(map[string]any{
		"exp": time.Now().Add(time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]string{
			"chatgpt_account_id": "shared-workspace", "chatgpt_user_id": user,
		},
	})
	return "header." + base64.RawURLEncoding.EncodeToString(body) + ".signature"
}

func TestSharedWorkspaceUsersHaveSeparateHistoryIdentity(t *testing.T) {
	alice, err := (Tokens{AccessToken: workspaceJWT("alice"), RefreshToken: "dummy-a"}).Profile()
	if err != nil {
		t.Fatal(err)
	}
	bob, err := (Tokens{AccessToken: workspaceJWT("bob"), RefreshToken: "dummy-b"}).Profile()
	if err != nil {
		t.Fatal(err)
	}
	if alice.AccountID != bob.AccountID || alice.Scope() == bob.Scope() {
		t.Fatal("shared workspace users were treated as one quota-history identity")
	}
}

func TestSameAccountLoginAlsoPreventsOldRequestRecovery(t *testing.T) {
	manager, err := NewManager(filepath.Join(t.TempDir(), "auth.json"), newHTTPClient(), DefaultTokenURL)
	if err != nil {
		t.Fatal(err)
	}
	tokens := Tokens{AccessToken: workspaceJWT("alice"), RefreshToken: "dummy-one"}
	if _, err := manager.Replace(context.Background(), tokens); err != nil {
		t.Fatal(err)
	}
	old, _ := manager.Current(context.Background())
	tokens.RefreshToken = "dummy-two"
	if _, err := manager.Replace(context.Background(), tokens); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.Recover(context.Background(), old); err == nil {
		t.Fatal("old request recovered across a new login generation")
	}
}

func TestPostRenameDurabilityFailurePublishesDiskAccount(t *testing.T) {
	manager, err := NewManager(filepath.Join(t.TempDir(), "auth.json"), newHTTPClient(), DefaultTokenURL)
	if err != nil {
		t.Fatal(err)
	}
	manager.persist = func(path string, body []byte) error {
		if err := storage.AtomicWrite(path, body); err != nil {
			return err
		}
		return &storage.CommitError{Err: errors.New("simulated directory sync failure")}
	}
	tokens := Tokens{AccessToken: workspaceJWT("alice"), RefreshToken: "dummy"}
	identity, err := manager.Replace(context.Background(), tokens)
	var committed *storage.CommitError
	if !errors.As(err, &committed) || !identity.CredentialConfigured {
		t.Fatal("committed failure was hidden")
	}
	profile, err := manager.Profile()
	if err != nil || profile.Scope() != identity.Scope() {
		t.Fatal("live account did not follow the committed file")
	}
}
