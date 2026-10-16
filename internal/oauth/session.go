package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/sortedcord/codex-proxy/internal/apikey"
	"github.com/sortedcord/codex-proxy/internal/config"
	"github.com/sortedcord/codex-proxy/internal/httputil"
)

var (
	ErrInvalidState            = errors.New("invalid OAuth state or callback")
	ErrLoginInProgress         = errors.New("login completion already in progress")
	ErrSessionUsed             = errors.New("login session already used; start a new attempt")
	ErrSessionExpired          = errors.New("login session expired; start a new attempt")
	ErrSessionClosed           = errors.New("login service is closed")
	ErrActivation              = errors.New("account activation failed")
	ErrAccountIdentityMismatch = errors.New("target account belongs to a different ChatGPT account")
	ErrAccountAlreadyLinked    = errors.New("ChatGPT account is already linked to another proxy account")
	ErrSessionNotFound         = errors.New("login session not found")
)

// SessionOptions controls account selection and optional key issuance for an OAuth session.
type SessionOptions struct {
	AccountID string `json:"account_id,omitempty"`
	Replace   bool   `json:"replace,omitempty"`
	IssueKey  string `json:"issue_key,omitempty"`
}

// SessionInfo describes a created login session.
type SessionInfo struct {
	ID        string    `json:"id"`
	AuthURL   string    `json:"login_url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// AccountConfigResult describes the result of committing an account during login.
type AccountConfigResult struct {
	AccountID string
	KeyID     string
	KeySecret string
}

// IssuedKey contains a newly generated API key secret for one-time retrieval.
type IssuedKey struct {
	ID     string `json:"id"`
	Label  string `json:"label,omitempty"`
	Secret string `json:"secret"`
}

// SessionStatus reports the state of a login session.
type SessionStatus struct {
	ID        string     `json:"id"`
	Status    string     `json:"status"`
	AuthURL   string     `json:"login_url,omitempty"`
	ExpiresAt time.Time  `json:"expires_at"`
	AccountID string     `json:"account_id,omitempty"`
	Email     string     `json:"email,omitempty"`
	Name      string     `json:"name,omitempty"`
	Key       *IssuedKey `json:"key,omitempty"`
	Error     string     `json:"error,omitempty"`
}

// ActivateWithOptionsFunc is the extended activation hook called when authorization succeeds.
type ActivateWithOptionsFunc func(ctx context.Context, opts SessionOptions, tokens Tokens) (Profile, AccountConfigResult, error)

type LoginOptions struct {
	Client              *http.Client
	AuthorizeURL        string
	TokenURL            string
	CallbackAddresses   []string
	Lifetime            time.Duration
	Activate            func(context.Context, Tokens) (Profile, error)
	ActivateWithOptions ActivateWithOptionsFunc
}

type LoginInfo struct {
	LoginURL  string    `json:"login_url"`
	ExpiresAt time.Time `json:"expires_at"`
	Status    string    `json:"status"`
}

type loginSession struct {
	id          string
	opts        SessionOptions
	state       string
	verifier    string
	redirectURI string
	info        LoginInfo
	listener    net.Listener
	server      *http.Server
	timer       *time.Timer
	status      string
	accountID   string
	email       string
	name        string
	key         *IssuedKey
	err         string
}

type SessionManager struct {
	mu        sync.Mutex
	options   LoginOptions
	active    *loginSession
	sessions  map[string]*loginSession
	ctx       context.Context
	cancel    context.CancelFunc
	work      sync.WaitGroup
	closed    bool
	closeOnce sync.Once
}

func NewSessionManager(ctx context.Context, options LoginOptions) (*SessionManager, error) {
	if ctx == nil || options.Client == nil || (options.Activate == nil && options.ActivateWithOptions == nil) {
		return nil, errors.New("login lifecycle, client and activation callback are required")
	}
	if options.ActivateWithOptions == nil && options.Activate != nil {
		oldActivate := options.Activate
		options.ActivateWithOptions = func(ctx context.Context, opts SessionOptions, tokens Tokens) (Profile, AccountConfigResult, error) {
			prof, err := oldActivate(ctx, tokens)
			return prof, AccountConfigResult{AccountID: prof.AccountID}, err
		}
	}
	if options.AuthorizeURL == "" {
		options.AuthorizeURL = "https://auth.openai.com/oauth/authorize"
	}
	if options.TokenURL == "" {
		options.TokenURL = DefaultTokenURL
	}
	if len(options.CallbackAddresses) == 0 {
		options.CallbackAddresses = []string{"127.0.0.1:1455", "127.0.0.1:1457"}
	}
	if options.Lifetime == 0 {
		options.Lifetime = 5 * time.Minute
	}
	if options.Lifetime <= 0 {
		return nil, errors.New("login lifetime must be positive")
	}
	for _, endpoint := range []string{options.AuthorizeURL, options.TokenURL} {
		if err := config.ValidateEndpoint(endpoint); err != nil {
			return nil, err
		}
	}
	client := *options.Client
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	options.Client = &client
	owned, cancel := context.WithCancel(ctx)
	return &SessionManager{
		options:  options,
		sessions: make(map[string]*loginSession),
		ctx:      owned,
		cancel:   cancel,
	}, nil
}

func randomString() (string, error) {
	var bytes [32]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", errors.New("cannot create OAuth entropy")
	}
	return base64.RawURLEncoding.EncodeToString(bytes[:]), nil
}

func generateSessionID() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", errors.New("cannot generate session ID")
	}
	return "sess_" + hex.EncodeToString(buf[:]), nil
}

func bindCallbackListener(addresses []string) (net.Listener, error) {
	for _, address := range addresses {
		host, _, err := net.SplitHostPort(address)
		if err != nil || host != "127.0.0.1" {
			return nil, errors.New("callback must bind IPv4 loopback")
		}
		listener, err := net.Listen("tcp4", address)
		if err == nil {
			return listener, nil
		}
	}
	return nil, errors.New("OAuth callback ports are unavailable")
}

func buildAuthorizeURL(authorizeURL, redirectURI, state, verifier string) string {
	challenge := sha256.Sum256([]byte(verifier))
	parameters := url.Values{
		"response_type":              {"code"},
		"client_id":                  {ClientID},
		"redirect_uri":               {redirectURI},
		"scope":                      {"openid profile email offline_access api.connectors.read api.connectors.invoke"},
		"state":                      {state},
		"code_challenge":             {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method":      {"S256"},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
		"originator":                 {"codex_cli_rs"},
	}
	return authorizeURL + "?" + parameters.Encode()
}

// BeginWithOptions initiates a login attempt with custom session options.
func (m *SessionManager) BeginWithOptions(ctx context.Context, opts SessionOptions) (SessionInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx.Err() != nil {
		return SessionInfo{}, ErrSessionClosed
	}
	if err := ctx.Err(); err != nil {
		return SessionInfo{}, err
	}

	now := time.Now()
	if m.active != nil {
		if m.active.info.Status == "exchanging" {
			return SessionInfo{}, ErrLoginInProgress
		}
		if m.active.info.Status == "pending" && now.Before(m.active.info.ExpiresAt) {
			if m.active.opts == opts {
				return SessionInfo{
					ID:        m.active.id,
					AuthURL:   m.active.info.LoginURL,
					ExpiresAt: m.active.info.ExpiresAt,
				}, nil
			}
			return SessionInfo{}, ErrLoginInProgress
		}
		m.cleanup(m.active)
		m.active = nil
	}

	state, err := randomString()
	if err != nil {
		return SessionInfo{}, err
	}
	verifier, err := randomString()
	if err != nil {
		return SessionInfo{}, err
	}
	sid, err := generateSessionID()
	if err != nil {
		return SessionInfo{}, err
	}
	listener, err := bindCallbackListener(m.options.CallbackAddresses)
	if err != nil {
		return SessionInfo{}, err
	}
	redirectURI := "http://" + listener.Addr().String() + "/auth/callback"
	session := &loginSession{
		id:          sid,
		opts:        opts,
		state:       state,
		verifier:    verifier,
		redirectURI: redirectURI,
		listener:    listener,
		status:      "pending",
		info: LoginInfo{
			LoginURL:  buildAuthorizeURL(m.options.AuthorizeURL, redirectURI, state, verifier),
			ExpiresAt: time.Now().UTC().Add(m.options.Lifetime),
			Status:    "pending",
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) { m.callback(session, w, r) })
	session.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      35 * time.Second,
		IdleTimeout:       5 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}
	session.server.SetKeepAlivesEnabled(false)

	session.timer = time.AfterFunc(m.options.Lifetime, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if session.info.Status == "pending" {
			session.info.Status = "expired"
			session.status = "expired"
			m.cleanup(session)
		}
	})

	m.active = session
	m.sessions[sid] = session

	m.work.Add(1)
	go func() { defer m.work.Done(); _ = session.server.Serve(listener) }()

	return SessionInfo{
		ID:        sid,
		AuthURL:   session.info.LoginURL,
		ExpiresAt: session.info.ExpiresAt,
	}, nil
}

// Begin initiates a default login attempt for backward compatibility.
func (m *SessionManager) Begin(ctx context.Context) (LoginInfo, error) {
	info, err := m.BeginWithOptions(ctx, SessionOptions{})
	if err != nil {
		return LoginInfo{}, err
	}
	return LoginInfo{
		LoginURL:  info.AuthURL,
		ExpiresAt: info.ExpiresAt,
		Status:    "pending",
	}, nil
}

func (m *SessionManager) cleanup(session *loginSession) {
	if session.timer != nil {
		session.timer.Stop()
	}
	if session.listener != nil {
		_ = session.listener.Close()
	}
}

// GetSession returns the current status and metadata of a login session.
func (m *SessionManager) GetSession(id string) (SessionStatus, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[id]
	if !ok {
		return SessionStatus{}, false
	}
	return SessionStatus{
		ID:        sess.id,
		Status:    sess.status,
		AuthURL:   sess.info.LoginURL,
		ExpiresAt: sess.info.ExpiresAt,
		AccountID: sess.accountID,
		Email:     sess.email,
		Name:      sess.name,
		Key:       sess.key,
		Error:     sess.err,
	}, true
}

// ConsumeSessionKey clears an issued API key from the session after retrieval.
func (m *SessionManager) ConsumeSessionKey(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sess, ok := m.sessions[id]; ok {
		sess.key = nil
	}
}

func (m *SessionManager) claimSessionByID(ctx context.Context, sessionID, state string) (*loginSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx.Err() != nil {
		return nil, ErrSessionClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var session *loginSession
	if sessionID != "" {
		var ok bool
		session, ok = m.sessions[sessionID]
		if !ok {
			return nil, ErrSessionNotFound
		}
	} else {
		session = m.active
	}
	if session == nil || subtle.ConstantTimeCompare([]byte(session.state), []byte(state)) != 1 {
		return nil, ErrInvalidState
	}
	if session.info.Status == "exchanging" {
		return nil, ErrLoginInProgress
	}
	expired := session.info.Status == "expired" || time.Now().After(session.info.ExpiresAt)
	if expired {
		session.info.Status = "expired"
		session.status = "expired"
		m.cleanup(session)
		return nil, ErrSessionExpired
	}
	if session.info.Status != "pending" {
		return nil, ErrSessionUsed
	}
	session.info.Status = "exchanging"
	session.status = "exchanging"
	m.cleanup(session)
	m.work.Add(1)
	return session, nil
}

// CompleteWithSessionID completes the OAuth exchange for a specific session ID.
func (m *SessionManager) CompleteWithSessionID(ctx context.Context, sessionID, code, state string) (Profile, AccountConfigResult, error) {
	if code == "" || len(code) > 8192 {
		return Profile{}, AccountConfigResult{}, ErrInvalidState
	}
	session, err := m.claimSessionByID(ctx, sessionID, state)
	if err != nil {
		return Profile{}, AccountConfigResult{}, err
	}
	defer m.work.Done()

	exchangeCtx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer cancel()

	tokens, err := m.exchange(exchangeCtx, session, code)
	var identity Profile
	var result AccountConfigResult
	if err == nil {
		identity, result, err = m.options.ActivateWithOptions(exchangeCtx, session.opts, tokens)
		if err != nil && !identity.CredentialConfigured {
			err = errors.Join(ErrActivation, err)
		}
	}

	m.mu.Lock()
	if identity.CredentialConfigured {
		session.info.Status = "completed"
		session.status = "configured"
		session.accountID = result.AccountID
		session.email = identity.Email
		session.name = identity.Name
		if result.KeySecret != "" {
			session.key = &IssuedKey{
				ID:     result.KeyID,
				Label:  session.opts.IssueKey,
				Secret: result.KeySecret,
			}
		}
	} else {
		session.info.Status = "failed"
		session.status = "failed"
		if err != nil {
			session.err = err.Error()
		}
	}
	m.mu.Unlock()

	return identity, result, err
}

func (m *SessionManager) Complete(ctx context.Context, code, state string) (Profile, error) {
	m.mu.Lock()
	activeID := ""
	if m.active != nil {
		activeID = m.active.id
	}
	m.mu.Unlock()

	prof, _, err := m.CompleteWithSessionID(ctx, activeID, code, state)
	return prof, err
}

func (m *SessionManager) exchange(ctx context.Context, session *loginSession, code string) (Tokens, error) {
	parameters := url.Values{
		"grant_type": {"authorization_code"}, "client_id": {ClientID}, "code": {code},
		"redirect_uri": {session.redirectURI}, "code_verifier": {session.verifier},
	}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		m.options.TokenURL,
		strings.NewReader(parameters.Encode()),
	)
	if err != nil {
		return Tokens{}, errors.New("invalid OAuth request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := m.options.Client.Do(req)
	if err != nil {
		return Tokens{}, errors.New("OAuth token exchange failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Tokens{}, fmt.Errorf("OAuth token exchange returned HTTP %d", resp.StatusCode)
	}
	body, err := httputil.ReadBounded(resp.Body, 1<<20)
	if err != nil {
		return Tokens{}, errors.New("cannot read bounded OAuth token response")
	}
	tokens := Tokens{}
	if err := json.Unmarshal(body, &tokens); err != nil {
		return Tokens{}, errors.New("invalid OAuth token response")
	}
	if _, err := tokens.Profile(); err != nil {
		return Tokens{}, err
	}
	return tokens, nil
}

func (m *SessionManager) CompleteURL(ctx context.Context, raw string) (Profile, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return Profile{}, ErrInvalidState
	}
	m.mu.Lock()
	session := m.active
	valid := session != nil && parsed.Scheme == "http" && parsed.User == nil
	if valid {
		valid = rawCallbackOrigin(parsed) == session.redirectURI && parsed.Fragment == ""
	}
	m.mu.Unlock()
	if !valid {
		return Profile{}, ErrInvalidState
	}
	values, err := url.ParseQuery(parsed.RawQuery)
	validParameters := err == nil && len(values["code"]) == 1 && len(values["state"]) == 1
	if !validParameters {
		return Profile{}, ErrInvalidState
	}
	if values.Get("error") != "" {
		return Profile{}, ErrInvalidState
	}
	return m.Complete(ctx, values.Get("code"), values.Get("state"))
}

func rawCallbackOrigin(parsed *url.URL) string {
	return parsed.Scheme + "://" + parsed.Host + parsed.EscapedPath()
}

func (m *SessionManager) callback(session *loginSession, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	expected, _ := url.Parse(session.redirectURI)
	if r.Method != http.MethodGet || r.Host != expected.Host {
		http.Error(w, "Invalid callback request", http.StatusBadRequest)
		return
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(values["state"]) != 1 {
		http.Error(w, "Invalid callback parameters", http.StatusBadRequest)
		return
	}
	if subtle.ConstantTimeCompare([]byte(values.Get("state")), []byte(session.state)) != 1 {
		http.Error(w, "OAuth state mismatch", http.StatusBadRequest)
		return
	}
	if values.Get("error") != "" {
		m.mu.Lock()
		if session.info.Status == "pending" {
			session.info.Status = "failed"
			session.status = "failed"
			session.err = "authorization declined"
			m.cleanup(session)
		}
		m.mu.Unlock()
		http.Error(w, "Authorization declined. Start a new login attempt.", http.StatusBadRequest)
		return
	}
	if len(values["code"]) != 1 {
		http.Error(w, "Invalid callback code", http.StatusBadRequest)
		return
	}
	identity, _, err := m.CompleteWithSessionID(r.Context(), session.id, values.Get("code"), values.Get("state"))
	if err != nil {
		message := "Authorization failed. Start a new login attempt."
		if identity.CredentialConfigured {
			message = "Account activated, but save durability is uncertain. Check proxy status."
		}
		http.Error(w, message, http.StatusBadRequest)
		return
	}
	_, _ = io.WriteString(w, "Authorization complete. You may close this tab and return to the proxy.\n")
}

// LoginWithOptions authorizes ChatGPT credentials through a loopback callback using PKCE
// and state validation with custom session options, then saves them to path.
func LoginWithOptions(cfg config.Config, path string, opts SessionOptions, output io.Writer) error {
	client := &http.Client{Timeout: 30 * time.Second}
	var mintedResult AccountConfigResult

	manager, err := NewSessionManager(context.Background(), LoginOptions{
		Client: client,
		ActivateWithOptions: func(ctx context.Context, sessionOpts SessionOptions, tokens Tokens) (Profile, AccountConfigResult, error) {
			if err := ctx.Err(); err != nil {
				return Profile{}, AccountConfigResult{}, err
			}
			identity, err := tokens.Profile()
			if err != nil {
				return Profile{}, AccountConfigResult{}, err
			}

			var mintedSecret, mintedKeyID string
			var newKey *config.AccountKey
			if sessionOpts.IssueKey != "" {
				var genErr error
				mintedSecret, genErr = apikey.Generate()
				if genErr != nil {
					return Profile{}, AccountConfigResult{}, genErr
				}
				mintedKeyID, genErr = apikey.GenerateID()
				if genErr != nil {
					return Profile{}, AccountConfigResult{}, genErr
				}
				newKey = &config.AccountKey{
					ID:        mintedKeyID,
					Label:     sessionOpts.IssueKey,
					Hash:      apikey.Hash(mintedSecret),
					Last4:     apikey.Last4(mintedSecret),
					CreatedAt: time.Now().UTC(),
				}
			}

			targetID := sessionOpts.AccountID
			creds := config.Credentials{
				AccessToken:  tokens.AccessToken,
				RefreshToken: tokens.RefreshToken,
				IDToken:      tokens.IDToken,
				AccountID:    identity.AccountID,
				AccountEmail: identity.Email,
				AccountName:  identity.Name,
				PlanType:     identity.PlanType,
			}

			err = config.UpdateAccounts(path, func(accounts *[]config.Account) error {
				if targetID == "" {
					for _, da := range *accounts {
						if da.AccountID == identity.AccountID {
							targetID = da.ID
							break
						}
					}
					if targetID == "" {
						if len(*accounts) == 0 || (len(*accounts) == 1 && (*accounts)[0].ID == "default") {
							targetID = "default"
						} else {
							existingIDs := make([]string, 0, len(*accounts))
							for _, da := range *accounts {
								existingIDs = append(existingIDs, da.ID)
							}
							targetID = config.DeriveAccountID(identity.Email, existingIDs)
						}
					}
				}

				for _, da := range *accounts {
					if da.AccountID == identity.AccountID && da.ID != targetID {
						return ErrAccountAlreadyLinked
					}
					if da.ID == targetID && da.AccountID != "" && da.AccountID != identity.AccountID && !sessionOpts.Replace {
						return ErrAccountIdentityMismatch
					}
				}

				for i := range *accounts {
					if (*accounts)[i].ID == targetID {
						(*accounts)[i].ApplyCredentials(creds)
						if newKey != nil {
							(*accounts)[i].Keys = append((*accounts)[i].Keys, *newKey)
						}
						return nil
					}
				}
				var keys []config.AccountKey
				if newKey != nil {
					keys = append(keys, *newKey)
				}
				newAcc := config.Account{
					ID:      targetID,
					Label:   targetID,
					Enabled: true,
					Keys:    keys,
				}
				newAcc.ApplyCredentials(creds)
				*accounts = append(*accounts, newAcc)
				return nil
			})
			if err != nil {
				return Profile{}, AccountConfigResult{}, err
			}

			res := AccountConfigResult{
				AccountID: targetID,
				KeyID:     mintedKeyID,
				KeySecret: mintedSecret,
			}
			mintedResult = res
			return identity, res, nil
		},
	})
	if err != nil {
		return err
	}
	defer manager.Close()

	info, err := manager.BeginWithOptions(context.Background(), opts)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintln(output, "Open this URL in your browser to authorize Codex Proxy:")
	_, _ = fmt.Fprintf(output, "\n  %s\n\n", info.AuthURL)
	_, _ = fmt.Fprintln(output, "Waiting for browser authorization callback...")

	for {
		time.Sleep(500 * time.Millisecond)
		sess, ok := manager.GetSession(info.ID)
		if !ok || sess.Status == "failed" {
			errMsg := sess.Error
			if errMsg == "" {
				errMsg = "authorization failed"
			}
			return errors.New(errMsg)
		}
		if sess.Status == "expired" {
			return errors.New("authorization session expired")
		}
		if sess.Status == "configured" {
			_, _ = fmt.Fprintf(output, "Account %q configured successfully.\n", mintedResult.AccountID)
			if mintedResult.KeySecret != "" {
				_, _ = fmt.Fprintf(output, "\nCreated API key for account %q:\n", mintedResult.AccountID)
				_, _ = fmt.Fprintf(output, "  Key:   %s\n", mintedResult.KeySecret)
				_, _ = fmt.Fprintf(output, "  ID:    %s\n", mintedResult.KeyID)
				if opts.IssueKey != "" {
					_, _ = fmt.Fprintf(output, "  Label: %s\n", opts.IssueKey)
				}
				_, _ = fmt.Fprintln(output, "\nSave this secret key now - it cannot be displayed again.")
			}
			return nil
		}
	}
}

func (m *SessionManager) Close() {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		m.cancel()
		servers := make([]*http.Server, 0, len(m.sessions))
		for _, s := range m.sessions {
			m.cleanup(s)
			if s.server != nil {
				servers = append(servers, s.server)
			}
		}
		m.mu.Unlock()
		for _, srv := range servers {
			_ = srv.Close()
		}
		m.work.Wait()
	})
}
