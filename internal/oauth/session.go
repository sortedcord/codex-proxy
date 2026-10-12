package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
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

	"github.com/sortedcord/codex-proxy/internal/config"
)

var (
	ErrInvalidState    = errors.New("invalid OAuth state or callback")
	ErrLoginInProgress = errors.New("login completion already in progress")
	ErrSessionUsed     = errors.New("login session already used; start a new attempt")
	ErrSessionExpired  = errors.New("login session expired; start a new attempt")
	ErrSessionClosed   = errors.New("login service is closed")
	ErrActivation      = errors.New("account activation failed")
)

type LoginOptions struct {
	Client            *http.Client
	AuthorizeURL      string
	TokenURL          string
	CallbackAddresses []string
	Lifetime          time.Duration
	Activate          func(context.Context, Tokens) (Profile, error)
}

type LoginInfo struct {
	LoginURL  string    `json:"login_url"`
	ExpiresAt time.Time `json:"expires_at"`
	Status    string    `json:"status"`
}

type loginSession struct {
	state       string
	verifier    string
	redirectURI string
	info        LoginInfo
	listener    net.Listener
	server      *http.Server
	timer       *time.Timer
}

type SessionManager struct {
	mu        sync.Mutex
	options   LoginOptions
	active    *loginSession
	ctx       context.Context
	cancel    context.CancelFunc
	work      sync.WaitGroup
	closed    bool
	closeOnce sync.Once
}

func NewSessionManager(ctx context.Context, options LoginOptions) (*SessionManager, error) {
	if ctx == nil || options.Client == nil || options.Activate == nil {
		return nil, errors.New("login lifecycle, client and activation callback are required")
	}
	if options.AuthorizeURL == "" {
		options.AuthorizeURL = "https://auth.openai.com/oauth/authorize"
	}
	if options.TokenURL == "" {
		options.TokenURL = DefaultTokenURL
	}
	if len(options.CallbackAddresses) == 0 {
		// OpenAI's Codex client allowlists these ports; arbitrary ephemeral ports
		// are only supplied by tests with a fake authorization server.
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
	return &SessionManager{options: options, ctx: owned, cancel: cancel}, nil
}

func randomString() (string, error) {
	var bytes [32]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", errors.New("cannot create OAuth entropy")
	}
	return base64.RawURLEncoding.EncodeToString(bytes[:]), nil
}

func (m *SessionManager) Begin(ctx context.Context) (LoginInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx.Err() != nil {
		return LoginInfo{}, ErrSessionClosed
	}
	if err := ctx.Err(); err != nil {
		return LoginInfo{}, err
	}
	if session := m.active; session != nil {
		if session.info.Status == "exchanging" {
			return LoginInfo{}, ErrLoginInProgress
		}
		if session.info.Status == "pending" && time.Now().Before(session.info.ExpiresAt) {
			return session.info, nil
		}
		m.cleanup(session)
	}
	state, err := randomString()
	if err != nil {
		return LoginInfo{}, err
	}
	verifier, err := randomString()
	if err != nil {
		return LoginInfo{}, err
	}
	var listener net.Listener
	for _, address := range m.options.CallbackAddresses {
		host, _, err := net.SplitHostPort(address)
		if err != nil || host != "127.0.0.1" {
			return LoginInfo{}, errors.New("callback must bind IPv4 loopback")
		}
		listener, err = net.Listen("tcp4", address)
		if err == nil {
			break
		}
	}
	if listener == nil {
		return LoginInfo{}, errors.New("OAuth callback ports are unavailable")
	}
	redirectURI := "http://" + listener.Addr().String() + "/auth/callback"
	challenge := sha256.Sum256([]byte(verifier))
	parameters := url.Values{
		"response_type": {"code"}, "client_id": {ClientID}, "redirect_uri": {redirectURI},
		"scope": {"openid profile email offline_access api.connectors.read api.connectors.invoke"},
		"state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"}, "id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow": {"true"}, "originator": {"codex_cli_rs"},
	}
	session := &loginSession{
		state: state, verifier: verifier, redirectURI: redirectURI, listener: listener,
		info: LoginInfo{LoginURL: m.options.AuthorizeURL + "?" + parameters.Encode(),
			ExpiresAt: time.Now().UTC().Add(m.options.Lifetime), Status: "pending"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) { m.callback(session, w, r) })
	session.server = &http.Server{
		Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second,
		WriteTimeout: 35 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8 << 10,
	}
	session.server.SetKeepAlivesEnabled(false)
	m.active = session
	session.timer = time.AfterFunc(m.options.Lifetime, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if session.info.Status == "pending" {
			session.info.Status = "expired"
			m.cleanup(session)
		}
	})
	m.work.Add(1)
	go func() { defer m.work.Done(); _ = session.server.Serve(listener) }()
	return session.info, nil
}

func (m *SessionManager) cleanup(session *loginSession) {
	if session.timer != nil {
		session.timer.Stop()
	}
	// Stop accepting without killing the current browser's success response.
	if session.listener != nil {
		_ = session.listener.Close()
	}
}

func (m *SessionManager) Complete(ctx context.Context, code, state string) (Profile, error) {
	m.mu.Lock()
	if m.closed || m.ctx.Err() != nil {
		m.mu.Unlock()
		return Profile{}, ErrSessionClosed
	}
	if err := ctx.Err(); err != nil {
		m.mu.Unlock()
		return Profile{}, err
	}
	session := m.active
	if session == nil || subtle.ConstantTimeCompare([]byte(session.state), []byte(state)) != 1 {
		m.mu.Unlock()
		return Profile{}, ErrInvalidState
	}
	if session.info.Status == "exchanging" {
		m.mu.Unlock()
		return Profile{}, ErrLoginInProgress
	}
	expired := session.info.Status == "expired" || time.Now().After(session.info.ExpiresAt)
	if expired {
		session.info.Status = "expired"
		m.cleanup(session)
		m.mu.Unlock()
		return Profile{}, ErrSessionExpired
	}
	if session.info.Status != "pending" {
		m.mu.Unlock()
		return Profile{}, ErrSessionUsed
	}
	if code == "" || len(code) > 8192 {
		m.mu.Unlock()
		return Profile{}, ErrInvalidState
	}
	session.info.Status = "exchanging"
	m.cleanup(session)
	m.work.Add(1)
	m.mu.Unlock()
	defer m.work.Done()
	// Once a one-time code is posted, client disconnects must not abandon a
	// successful exchange/save. Shutdown still cancels and joins this bounded work.
	exchangeCtx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer cancel()
	tokens, err := m.exchange(exchangeCtx, session, code)
	var identity Profile
	if err == nil {
		identity, err = m.options.Activate(exchangeCtx, tokens)
		if err != nil && !identity.CredentialConfigured {
			err = errors.Join(ErrActivation, err)
		}
	}
	m.mu.Lock()
	if identity.CredentialConfigured {
		session.info.Status = "completed"
	} else {
		session.info.Status = "failed"
	}
	m.mu.Unlock()
	return identity, err
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
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Tokens{}, fmt.Errorf("OAuth token exchange returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
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
	identity, err := m.Complete(r.Context(), values.Get("code"), values.Get("state"))
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

func (m *SessionManager) Close() {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		m.cancel()
		var server *http.Server
		if m.active != nil {
			m.cleanup(m.active)
			server = m.active.server
		}
		m.mu.Unlock()
		if server != nil {
			_ = server.Close()
		}
		m.work.Wait()
	})
}
