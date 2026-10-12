package oauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sortedcord/codex-proxy/internal/httputil"
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
	expiresAt    time.Time
}

type authDocument struct {
	root    map[string]json.RawMessage
	tokens  map[string]json.RawMessage
	creds   Credentials
	profile Profile
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
	cachedDoc    authDocument
	cachedHash   [sha256.Size]byte
	cacheValid   bool
	published    atomic.Pointer[profileSnapshot]
}

// Non-secret immutable state lets cached status reads bypass an in-flight refresh.
type profileSnapshot struct {
	digest  [sha256.Size]byte
	profile Profile
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

func (a *Manager) Current(ctx context.Context) (Credentials, error) {
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

// Recovery must never replay an old request under a newly activated account.
func (a *Manager) Recover(ctx context.Context, rejected Credentials) (Credentials, bool, error) {
	return a.recover(ctx, rejected.AccessToken, &rejected)
}

func (a *Manager) recover(ctx context.Context, rejected string, expected *Credentials) (Credentials, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Credentials{}, false, err
	}
	doc, err := a.load()
	if err != nil {
		return Credentials{}, false, err
	}
	if expected != nil {
		accountChanged := doc.creds.AccountID != expected.AccountID
		loginChanged := doc.creds.Generation != expected.Generation
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
func (a *Manager) WaitForRefresh() {
	a.mu.Lock()
	a.mu.Unlock() //nolint:staticcheck // SA2001: locking joins the serialized refresh before shutdown.
}

// Snapshot binds a request before reading its body, without starting a refresh.
// It shares the content-validated cache with Current and Profile.
func (a *Manager) Snapshot(ctx context.Context) (Credentials, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Credentials{}, err
	}
	doc, err := a.load()
	if err != nil {
		return Credentials{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return doc.creds, nil
}

// Fresh reuses the request's snapshot unless it needs refresh. Recovery verifies
// its login generation, so even proactive refresh cannot adopt a switched account.
func (a *Manager) Fresh(ctx context.Context, snapshot Credentials) (Credentials, error) {
	if err := ctx.Err(); err != nil {
		return Credentials{}, err
	}
	if !needsRefresh(snapshot, time.Now()) {
		return snapshot, nil
	}
	creds, _, err := a.Recover(ctx, snapshot)
	return creds, err
}

func (a *Manager) load() (authDocument, error) {
	body, err := readAuthBody(a.path)
	if err != nil {
		return authDocument{}, err
	}
	// Metadata/inode checks alone cannot detect in-place, same-size rewrites
	// with preserved timestamps. Read bounded bytes and cache parsing by content.
	digest := sha256.Sum256(body)
	if !a.cacheValid || digest != a.cachedHash {
		doc, err := parseAuth(body)
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
		a.cachedDoc, a.cachedHash, a.cacheValid = doc, digest, true
		a.published.Store(&profileSnapshot{digest: digest, profile: doc.profile})
	}
	doc := a.cachedDoc
	// Refresh mutates map entries before persistence. Never alias the cache;
	// RawMessage values are immutable and replaced, not edited in place.
	doc.root = maps.Clone(doc.root)
	doc.tokens = maps.Clone(doc.tokens)
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
	defer func() { _ = resp.Body.Close() }()
	body, err := httputil.ReadBounded(resp.Body, 1<<20)
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
	a.cacheValid = false
	a.failedToken = ""
	a.failedAccess = ""
	a.refreshError = nil
	stored, err := a.load()
	return stored.creds, err
}

func readAuthBody(path string) ([]byte, error) {
	if err := storage.InspectFile(path); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot open Codex auth file %q; run codex login: %w", path, err)
	}
	defer func() { _ = file.Close() }() // Read-only descriptor; its data has already been consumed.
	body, err := httputil.ReadBounded(file, 1<<20)
	if err != nil {
		return nil, errors.New("cannot read Codex auth file")
	}
	return body, nil
}

func readAuth(path string) (authDocument, error) {
	body, err := readAuthBody(path)
	if err != nil {
		return authDocument{}, err
	}
	return parseAuth(body)
}

func parseAuth(body []byte) (authDocument, error) {
	doc := authDocument{
		root:   map[string]json.RawMessage{},
		tokens: map[string]json.RawMessage{},
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
	doc.profile = profile(doc.creds.AccountID, jsonString(doc.tokens["id_token"]), doc.creds.AccessToken)
	doc.creds.IdentityKey = doc.profile.Scope()
	doc.creds.LastRefresh, _ = time.Parse(time.RFC3339, jsonString(doc.root["last_refresh"]))
	doc.creds.expiresAt = tokenExpiry(doc.creds.AccessToken)
	return doc, nil
}

func tokenExpiry(token string) time.Time {
	claims := jwtClaims(token)
	var expiry int64
	if err := json.Unmarshal(claims["exp"], &expiry); err == nil && expiry > 0 {
		return time.Unix(expiry, 0)
	}
	return time.Time{}
}

func needsRefresh(creds Credentials, now time.Time) bool {
	expiry := creds.expiresAt
	if expiry.IsZero() {
		expiry = tokenExpiry(creds.AccessToken)
	}
	if !expiry.IsZero() {
		return expiry.Before(now.Add(5 * time.Minute))
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

func jsonString(raw json.RawMessage) string { return httputil.JSONString(raw) }

func setJSON(object map[string]json.RawMessage, key string, value any) {
	httputil.SetJSON(object, key, value)
}
