package oauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sortedcord/codex-proxy/internal/storage"
)

const (
	ClientID        = "app_EMoamEEZ73f0CkXaXp7hrann"
	DefaultTokenURL = "https://auth.openai.com/oauth/token"
)

var ErrUnavailable = errors.New("Codex credentials are not configured or are unavailable")

type Credentials struct {
	AccessToken  string    `json:"-"`
	RefreshToken string    `json:"-"`
	AccountID    string    `json:"account_id"`
	IdentityKey  string    `json:"-"`
	Generation   uint64    `json:"-"`
	LastRefresh  time.Time `json:"-"`
}

type authDocument struct {
	root   map[string]json.RawMessage
	tokens map[string]json.RawMessage
	creds  Credentials
}

type Manager struct {
	mu           sync.Mutex
	path         string
	accountID    string
	identityKey  string
	generation   uint64
	client       *http.Client
	tokenURL     string
	failedToken  string
	failedAccess string
	refreshError error
	persist      func(string, []byte) error
}

func NewManager(path string, client *http.Client, tokenURL string) (*Manager, error) {
	doc, err := readAuth(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if client == nil {
		return nil, errors.New("OAuth HTTP client is required")
	}
	return &Manager{
		path: path, accountID: doc.creds.AccountID, identityKey: doc.creds.IdentityKey, generation: 1,
		client: client, tokenURL: tokenURL, persist: atomicWrite,
	}, nil
}

func (a *Manager) current(ctx context.Context) (Credentials, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Credentials{}, err
	}
	doc, err := a.load()
	if err != nil {
		return Credentials{}, err
	}
	failedCredentials := a.failedToken == doc.creds.RefreshToken && a.failedAccess == doc.creds.AccessToken
	if failedCredentials && a.refreshError != nil {
		return Credentials{}, a.refreshError
	}
	if !needsRefresh(doc.creds, time.Now()) {
		return doc.creds, nil
	}
	return a.refresh(doc)
}

func (a *Manager) Current(ctx context.Context) (Credentials, error) { return a.current(ctx) }

// Recovery must never replay an old request under a newly activated account.
func (a *Manager) Recover(ctx context.Context, rejected Credentials) (Credentials, bool, error) {
	return a.recover(ctx, rejected.AccessToken, rejected)
}

func (a *Manager) recover(ctx context.Context, rejected string, expected ...Credentials) (Credentials, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Credentials{}, false, err
	}
	doc, err := a.load()
	if err != nil {
		return Credentials{}, false, err
	}
	if len(expected) > 0 {
		accountChanged := doc.creds.AccountID != expected[0].AccountID
		loginChanged := doc.creds.Generation != expected[0].Generation
		if accountChanged || loginChanged {
			return Credentials{}, false, errors.New("login changed during request; request was not replayed")
		}
	}
	if doc.creds.AccessToken != rejected {
		return doc.creds, false, nil
	}
	creds, err := a.refresh(doc)
	return creds, true, err
}

// Shutdown must not terminate an in-flight refresh after it rotates a token but
// before the new credentials reach disk. New requests are stopped first.
func (a *Manager) waitForRefresh() {
	a.mu.Lock()
	a.mu.Unlock()
}

func (a *Manager) WaitForRefresh() { a.waitForRefresh() }

func (a *Manager) load() (authDocument, error) {
	doc, err := readAuth(a.path)
	if err != nil {
		return doc, err
	}
	if a.accountID == "" {
		a.accountID = doc.creds.AccountID
		a.identityKey = doc.creds.IdentityKey
	}
	if doc.creds.AccountID != a.accountID || doc.creds.IdentityKey != a.identityKey {
		return doc, errors.New("Codex account changed; restart the proxy to use the new account")
	}
	doc.creds.Generation = a.generation
	return doc, nil
}

// The mutex coalesces refreshes inside this process. Codex does not honor our
// locks: run only one token-refreshing process per auth file (see README).
func (a *Manager) refresh(doc authDocument) (Credentials, error) {
	if doc.creds.RefreshToken == "" {
		return Credentials{}, errors.New("Codex refresh token missing; run codex login")
	}
	failedCredentials := a.failedToken == doc.creds.RefreshToken && a.failedAccess == doc.creds.AccessToken
	if failedCredentials && a.refreshError != nil {
		return Credentials{}, a.refreshError
	}

	// Finish rotating and persisting tokens even if the requesting client leaves.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	payload, err := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     ClientID,
		"refresh_token": doc.creds.RefreshToken,
	})
	if err != nil {
		return Credentials{}, err
	}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		a.tokenURL,
		bytes.NewReader(payload),
	)
	if err != nil {
		return Credentials{}, errors.New("invalid OAuth token endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return Credentials{}, errors.New("Codex token refresh transport failure")
	}
	defer resp.Body.Close()
	body, err := readBounded(resp.Body, 1<<20)
	if err != nil {
		return Credentials{}, errors.New("cannot read Codex token refresh response")
	}

	// Re-read before saving: never knowingly overwrite another writer's tokens,
	// a logout, an account switch, or unrelated metadata updates.
	latest, err := a.load()
	if err != nil {
		return Credentials{}, err
	}
	changed := latest.creds.AccessToken != doc.creds.AccessToken ||
		latest.creds.RefreshToken != doc.creds.RefreshToken
	if changed {
		return latest.creds, nil
	}
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("Codex token refresh rejected (HTTP %d); run codex login", resp.StatusCode)
		if permanentRefreshFailure(resp.StatusCode, body) {
			a.failedToken = doc.creds.RefreshToken
			a.failedAccess = doc.creds.AccessToken
			a.refreshError = err
		}
		return Credentials{}, err
	}

	update := struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
	}{}
	if err := json.Unmarshal(body, &update); err != nil || update.AccessToken == "" {
		return Credentials{}, errors.New("invalid Codex token refresh response")
	}
	setJSON(latest.tokens, "access_token", update.AccessToken)
	if update.RefreshToken != "" {
		setJSON(latest.tokens, "refresh_token", update.RefreshToken)
	}
	if update.IDToken != "" {
		setJSON(latest.tokens, "id_token", update.IDToken)
	}
	setJSON(latest.root, "last_refresh", time.Now().UTC().Format(time.RFC3339))
	setJSON(latest.root, "tokens", latest.tokens)
	encoded, err := json.MarshalIndent(latest.root, "", "  ")
	if err != nil {
		return Credentials{}, errors.New("cannot encode refreshed Codex credentials")
	}
	if err := a.persist(a.path, append(encoded, '\n')); err != nil {
		return Credentials{}, fmt.Errorf("cannot persist refreshed Codex credentials: %w", err)
	}
	a.failedToken = ""
	a.failedAccess = ""
	a.refreshError = nil
	stored, err := a.load()
	return stored.creds, err
}

func readAuth(path string) (authDocument, error) {
	doc := authDocument{
		root:   map[string]json.RawMessage{},
		tokens: map[string]json.RawMessage{},
	}
	if err := storage.InspectFile(path); err != nil {
		return doc, err
	}
	file, err := os.Open(path)
	if err != nil {
		return doc, fmt.Errorf("cannot open Codex auth file %q; run codex login: %w", path, err)
	}
	defer file.Close()
	body, err := readBounded(file, 1<<20)
	if err != nil {
		return doc, errors.New("cannot read Codex auth file")
	}
	if err := json.Unmarshal(body, &doc.root); err != nil || doc.root == nil {
		return doc, errors.New("invalid Codex auth JSON")
	}
	mode := jsonString(doc.root["auth_mode"])
	if mode != "" && mode != "chatgpt" {
		return doc, errors.New("only Codex ChatGPT OAuth credentials are supported; run codex login")
	}
	if err := json.Unmarshal(doc.root["tokens"], &doc.tokens); err != nil || doc.tokens == nil {
		return doc, errors.New("Codex file-backed OAuth tokens missing; run codex login")
	}
	doc.creds.AccessToken = jsonString(doc.tokens["access_token"])
	doc.creds.RefreshToken = jsonString(doc.tokens["refresh_token"])
	doc.creds.AccountID = jsonString(doc.tokens["account_id"])
	if doc.creds.AccountID == "" {
		claims := jwtClaims(jsonString(doc.tokens["id_token"]))
		authClaims := map[string]json.RawMessage{}
		_ = json.Unmarshal(claims["https://api.openai.com/auth"], &authClaims)
		doc.creds.AccountID = jsonString(authClaims["chatgpt_account_id"])
	}
	missingCredentials := doc.creds.AccessToken == "" || doc.creds.AccountID == ""
	invalidHeaders := strings.ContainsAny(doc.creds.AccessToken+doc.creds.AccountID, "\r\n")
	if missingCredentials || invalidHeaders {
		return doc, errors.New("Codex access token or account ID missing/invalid; run codex login")
	}
	doc.creds.IdentityKey = profile(doc.creds.AccountID, jsonString(doc.tokens["id_token"]), doc.creds.AccessToken).Scope()
	doc.creds.LastRefresh, _ = time.Parse(time.RFC3339, jsonString(doc.root["last_refresh"]))
	return doc, nil
}

func needsRefresh(creds Credentials, now time.Time) bool {
	claims := jwtClaims(creds.AccessToken)
	var expiry int64
	if err := json.Unmarshal(claims["exp"], &expiry); err == nil && expiry > 0 {
		return time.Unix(expiry, 0).Before(now.Add(5 * time.Minute))
	}
	return !creds.LastRefresh.IsZero() && creds.LastRefresh.Before(now.Add(-8*24*time.Hour))
}

// JWT claims are hints for scheduling/routing, not verified authentication.
// Only OpenAI validates the actual access token.
func jwtClaims(token string) map[string]json.RawMessage {
	claims := map[string]json.RawMessage{}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return claims
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return claims
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims == nil {
		return map[string]json.RawMessage{}
	}
	return claims
}

func permanentRefreshFailure(status int, body []byte) bool {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return true
	}
	for _, code := range []string{
		"invalid_grant", "refresh_token_expired", "refresh_token_reused", "refresh_token_invalidated",
	} {
		if bytes.Contains(body, []byte(code)) {
			return true
		}
	}
	return false
}

func atomicWrite(path string, data []byte) error { return storage.AtomicWrite(path, data) }

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errors.New("body exceeds size limit")
	}
	return body, nil
}

func jsonString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func setJSON(object map[string]json.RawMessage, key string, value any) {
	// Callers only supply JSON-encodable primitives or raw JSON maps.
	object[key], _ = json.Marshal(value)
}
