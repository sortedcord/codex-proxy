package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sortedcord/codex-proxy/internal/storage"
)

// Profile contains non-secret identity hints. JWT claims are not verified locally.
type Profile struct {
	CredentialConfigured bool   `json:"credential_configured"`
	AccountID            string `json:"account_id,omitempty"`
	Email                string `json:"email,omitempty"`
	Name                 string `json:"name,omitempty"`
	PlanType             string `json:"plan_type,omitempty"`
	IdentitySource       string `json:"identity_source,omitempty"`
	PlanSource           string `json:"plan_source,omitempty"`
	IdentityKey          string `json:"-"`
}

// A ChatGPT workspace can contain multiple users. Histories bind both identities
// when the provider reports a user ID; legacy opaque credentials fall back to account ID.
func (p Profile) Scope() string {
	if p.IdentityKey != "" {
		return p.IdentityKey
	}
	return p.AccountID
}

// Tokens are accepted only from the trusted OAuth exchange, never from API input.
type Tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
}

func (a *Manager) Profile() (Profile, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	doc, err := a.load()
	if err != nil {
		return Profile{}, ErrUnavailable
	}
	return profile(doc.creds.AccountID, jsonString(doc.tokens["id_token"]), doc.creds.AccessToken), nil
}

func profile(accountID, idToken, accessToken string) Profile {
	result := Profile{CredentialConfigured: true, AccountID: accountID, IdentitySource: "token_claims_unverified"}
	var userID, subject string
	for _, token := range []string{idToken, accessToken} {
		claims := jwtClaims(token)
		if result.Email == "" {
			result.Email = jsonString(claims["email"])
		}
		if result.Name == "" {
			result.Name = jsonString(claims["name"])
		}
		auth := map[string]json.RawMessage{}
		_ = json.Unmarshal(claims["https://api.openai.com/auth"], &auth)
		if userID == "" {
			userID = jsonString(auth["chatgpt_user_id"])
		}
		if userID == "" {
			userID = jsonString(auth["user_id"])
		}
		if subject == "" {
			subject = jsonString(claims["sub"])
		}
		if result.PlanType == "" {
			result.PlanType = jsonString(auth["chatgpt_plan_type"])
		}
		user := map[string]json.RawMessage{}
		_ = json.Unmarshal(claims["https://api.openai.com/profile"], &user)
		if result.Email == "" {
			result.Email = jsonString(user["email"])
		}
	}
	if result.PlanType != "" {
		result.PlanSource = "token_claims_unverified"
	}
	if userID == "" {
		userID = subject
	}
	if userID != "" {
		digest := sha256.Sum256([]byte(accountID + "\x00" + userID))
		result.IdentityKey = hex.EncodeToString(digest[:])
	}
	return result
}

func (tokens Tokens) Profile() (Profile, error) {
	var accountID string
	for _, token := range []string{tokens.IDToken, tokens.AccessToken} {
		claims := jwtClaims(token)
		auth := map[string]json.RawMessage{}
		_ = json.Unmarshal(claims["https://api.openai.com/auth"], &auth)
		id := jsonString(auth["chatgpt_account_id"])
		if id == "" {
			continue
		}
		if accountID != "" && accountID != id {
			return Profile{}, errors.New("OAuth returned conflicting account identities")
		}
		accountID = id
	}
	missing := tokens.AccessToken == "" || tokens.RefreshToken == "" || accountID == ""
	invalid := strings.ContainsAny(tokens.AccessToken+accountID, "\r\n")
	if missing || invalid {
		return Profile{}, errors.New("OAuth returned incomplete account credentials")
	}
	return profile(accountID, tokens.IDToken, tokens.AccessToken), nil
}

// Replace is serialized with refresh. A commit publishes the new account before
// releasing the lock; old requests cannot recover with the replacement's tokens.
func (a *Manager) Replace(ctx context.Context, tokens Tokens) (Profile, error) {
	identity, err := tokens.Profile()
	if err != nil {
		return Profile{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Profile{}, err
	}
	if err := os.MkdirAll(filepath.Dir(a.path), 0700); err != nil {
		return Profile{}, errors.New("cannot prepare credential directory")
	}
	body, err := json.MarshalIndent(map[string]any{
		"auth_mode": "chatgpt", "OPENAI_API_KEY": nil,
		"last_refresh": time.Now().UTC().Format(time.RFC3339),
		"tokens": map[string]string{
			"access_token": tokens.AccessToken, "refresh_token": tokens.RefreshToken,
			"id_token": tokens.IDToken, "account_id": identity.AccountID,
		},
	}, "", "  ")
	if err != nil {
		return Profile{}, errors.New("cannot encode login credentials")
	}
	err = a.persist(a.path, append(body, '\n'))
	var committed *storage.CommitError
	if err != nil && !errors.As(err, &committed) {
		return Profile{}, errors.New("cannot persist login credentials")
	}
	a.accountID = identity.AccountID
	a.identityKey = identity.Scope()
	a.generation++
	a.failedAccess, a.failedToken, a.refreshError = "", "", nil
	return identity, err
}
