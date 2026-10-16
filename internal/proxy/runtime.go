package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sortedcord/codex-proxy/internal/apikey"
	"github.com/sortedcord/codex-proxy/internal/config"
	"github.com/sortedcord/codex-proxy/internal/httputil"
	"github.com/sortedcord/codex-proxy/internal/oauth"
	"github.com/sortedcord/codex-proxy/internal/quota"
	"github.com/sortedcord/codex-proxy/internal/status"
	"github.com/sortedcord/codex-proxy/internal/storage"
)

type accountStatus interface {
	Latest() (quota.Snapshot, bool, status.PollInfo)
	Stale(quota.Snapshot, status.PollInfo) bool
	Query(status.Query) (status.Result, error)
	ServeHTTP(http.ResponseWriter, *http.Request)
	Close() error
}

type statusAdapter struct {
	svc       *status.Service
	namespace string
}

func (a *statusAdapter) Latest() (quota.Snapshot, bool, status.PollInfo) {
	return a.svc.Latest(a.namespace)
}

func (a *statusAdapter) Stale(snap quota.Snapshot, poll status.PollInfo) bool {
	return a.svc.Stale(snap, poll)
}

func (a *statusAdapter) Query(q status.Query) (status.Result, error) {
	return a.svc.Query(a.namespace, q)
}

func (a *statusAdapter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/status/limit" {
		snap, avail, poll := a.svc.Latest(a.namespace)
		if !avail {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{
					"type":    "server_error",
					"code":    "quota_unavailable",
					"message": "quota has not been observed for the account",
				},
				"polling": poll,
			})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(struct {
			quota.Snapshot
			Polling status.PollInfo `json:"polling"`
			Stale   bool            `json:"stale"`
		}{
			Snapshot: snap,
			Polling:  poll,
			Stale:    a.svc.Stale(snap, poll),
		})
		return
	}
	if r.URL.Path == "/status/usage" {
		q, err := status.ParseQuery(r.URL.RawQuery)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		res, err := a.svc.Query(a.namespace, q)
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "quota history is unavailable"})
			return
		}
		_, _, poll := a.svc.Latest(a.namespace)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(struct {
			status.Result
			Polling status.PollInfo `json:"polling"`
		}{
			Result:  res,
			Polling: poll,
		})
		return
	}
}

func (a *statusAdapter) Close() error {
	return nil
}

type accountRuntime struct {
	id        string
	label     string
	enabled   bool
	proxy     *proxy
	auth      *oauth.Manager
	quota     *quota.Fetcher
	status    accountStatus
	namespace string
}

type runtimeRegistry struct {
	accounts map[string]*accountRuntime
	keys     *apikey.Registry
	lastUsed sync.Map
}

func (reg *runtimeRegistry) recordKeyUsage(keyID string) {
	if keyID == "" {
		return
	}
	reg.lastUsed.Store(keyID, time.Now().UTC())
}

func (reg *runtimeRegistry) getKeyLastUsed(keyID string) *time.Time {
	if val, ok := reg.lastUsed.Load(keyID); ok {
		t := val.(time.Time)
		return &t
	}
	return nil
}

type Runtime struct {
	registry   atomic.Pointer[runtimeRegistry]
	cfg        config.Config
	configPath string
	logger     *slog.Logger
	client     *http.Client
	login      *oauth.SessionManager
	lock       *storage.Lock
	cancel     context.CancelFunc
	ctx        context.Context
	closeOnce  sync.Once
	closeErr   error
	mu         sync.Mutex

	adminKeyHash string
	adminAPIKey  string
	adminHandler http.Handler
	envAccounts  map[string]bool
	persist      func(string, func(*[]config.Account) error) error

	// Backward-compatible fields
	auth       *oauth.Manager
	activeAuth atomic.Pointer[oauth.Manager]
	inference  *proxy
	status     *status.Service
}

func (r *Runtime) getAuth() *oauth.Manager {
	if m := r.activeAuth.Load(); m != nil {
		return m
	}
	return r.auth
}

func (r *Runtime) setAuth(m *oauth.Manager) {
	if m != nil {
		r.activeAuth.Store(m)
	}
	r.auth = m
}

func (r *Runtime) isEnvAccount(id string) bool {
	if r.envAccounts == nil {
		return false
	}
	return r.envAccounts[id]
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func accountNamespace(acc config.Account) string {
	if acc.AccountID != "" {
		return acc.AccountID
	}
	if acc.AccessToken == "" && acc.RefreshToken == "" {
		return "unconfigured:" + acc.ID
	}
	encoded, _ := json.Marshal([]string{acc.AccessToken, acc.RefreshToken, acc.IDToken})
	digest := sha256.Sum256(encoded)
	return "unresolved:" + hex.EncodeToString(digest[:])
}

// NewRuntime constructs and initializes the multi-account runtime.
func NewRuntime(ctx context.Context, cfg config.Config, logger *slog.Logger) (*Runtime, error) {
	return newRuntime(
		ctx,
		cfg,
		logger,
		oauth.LoginOptions{},
	)
}

func newRuntime(
	ctx context.Context,
	cfg config.Config,
	logger *slog.Logger,
	loginOptions oauth.LoginOptions,
) (*Runtime, error) {
	if ctx == nil || logger == nil {
		return nil, errors.New("runtime requires a context and logger")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var lock *storage.Lock
	if cfg.AuthFile != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.AuthFile), 0o700); err != nil {
			return nil, err
		}
		if err := storage.InspectFile(cfg.AuthFile); err != nil {
			return nil, err
		}
		parent, err := filepath.EvalSymlinks(filepath.Dir(cfg.AuthFile))
		if err != nil {
			return nil, err
		}
		cfg.AuthFile = filepath.Join(parent, filepath.Base(cfg.AuthFile))
		lock, err = storage.Acquire(cfg.AuthFile + ".codex-proxy.lock")
		if err != nil {
			return nil, err
		}
	}

	owned, cancel := context.WithCancel(ctx)
	client := newHTTPClient()
	tokenURL := loginOptions.TokenURL
	if tokenURL == "" {
		tokenURL = oauth.DefaultTokenURL
	}

	configPath := cfg.ConfigPath
	if configPath == "" {
		configPath = cfg.ConfigFile
	}
	if configPath == "" {
		if cfg.AuthFile != "" {
			configPath = filepath.Join(filepath.Dir(cfg.AuthFile), "config.json")
		} else {
			home, _ := os.UserHomeDir()
			configPath = filepath.Join(home, ".config", "codex-proxy", "config.json")
		}
	}

	var err error
	var defaultAuthMgr *oauth.Manager
	if cfg.AuthFile != "" {
		defaultAuthMgr, err = oauth.NewManager(cfg.AuthFile, client, tokenURL)
		if err != nil && !os.IsNotExist(err) {
			if lock != nil {
				_ = lock.Close()
			}
			cancel()
			return nil, err
		}
	}

	rt := &Runtime{
		cfg:          cfg,
		configPath:   configPath,
		logger:       logger,
		client:       client,
		lock:         lock,
		cancel:       cancel,
		ctx:          owned,
		persist:      config.UpdateAccounts,
		adminKeyHash: cfg.AdminKeyHash,
		adminAPIKey:  cfg.AdminAPIKey,
		envAccounts:  make(map[string]bool),
	}
	rt.setAuth(defaultAuthMgr)
	if defaultAuthMgr != nil {
		rt.inference = newProxy(cfg, defaultAuthMgr, client, logger)
	}
	if rt.adminAPIKey == "" {
		rt.adminAPIKey = cfg.ManagementKey
	}

	// Login session manager only if management/admin is enabled
	if cfg.ManagementKey != "" || rt.adminAPIKey != "" || rt.adminKeyHash != "" {
		loginOptions.Client = client
		loginOptions.ActivateWithOptions = rt.activate
		loginOptions.Activate = func(ctx context.Context, tokens oauth.Tokens) (oauth.Profile, error) {
			prof, _, err := rt.activate(ctx, oauth.SessionOptions{}, tokens)
			return prof, err
		}
		rt.login, err = oauth.NewSessionManager(owned, loginOptions)
		if err != nil {
			_ = rt.Close()
			return nil, err
		}
	}

	rt.adminHandler = rt.setupAdminMux()

	// Initial registry load
	if err := rt.reloadRegistryLocked(); err != nil {
		_ = rt.Close()
		return nil, err
	}

	// Backward-compatible single-account status service if ManagementKey configured
	if cfg.ManagementKey != "" && defaultAuthMgr != nil {
		fetcher := &quota.Fetcher{
			Client:        client,
			Endpoint:      cfg.UsageURL,
			UserAgent:     cfg.UserAgent,
			ClientVersion: cfg.ClientVersion,
		}
		rt.status, err = status.NewService(owned, status.Options{
			Dir:        cfg.HistoryDir,
			MaxSamples: cfg.HistoryMaxSamples,
			Interval:   cfg.PollInterval,
			Account: func() (oauth.Profile, error) {
				if a := rt.getAuth(); a != nil {
					return a.Profile()
				}
				return oauth.Profile{}, oauth.ErrUnavailable
			},
			Fetch: func(ctx context.Context) (quota.Snapshot, error) {
				if a := rt.getAuth(); a != nil {
					creds, err := a.Current(ctx)
					if err != nil {
						return quota.Snapshot{}, err
					}
					return fetcher.Fetch(ctx, creds)
				}
				return quota.Snapshot{}, errors.New("no active credentials")
			},
		})
		if err != nil {
			_ = rt.Close()
			return nil, err
		}
	}

	return rt, nil
}

func (r *Runtime) isManagementEnabled() bool {
	return r.cfg.ManagementKey != "" || r.adminAPIKey != "" || r.adminKeyHash != ""
}

func (r *Runtime) prepareAccount(accCfg config.Account, previous *accountRuntime) (*accountRuntime, error) {
	accProxyCfg := r.cfg
	accProxyCfg.APIKey = "" // Outer Runtime handles request key validation

	targetID := accCfg.ID
	tokenURL := oauth.DefaultTokenURL

	var authMgr *oauth.Manager
	var err error

	switch {
	case accCfg.HasCredentials():
		creds := oauth.Credentials{
			AccessToken:  accCfg.AccessToken,
			RefreshToken: accCfg.RefreshToken,
			IDToken:      accCfg.IDToken,
			AccountID:    accCfg.AccountID,
		}
		authMgr, err = oauth.NewAccountManager(creds, r.client, tokenURL, r.persistTokenRefresh(targetID))
		if err != nil {
			return nil, err
		}
	case accCfg.ID == "default" && r.getAuth() != nil:
		authMgr = r.getAuth()
	case r.cfg.AuthFile != "" && accCfg.ID == "default":
		authMgr, err = oauth.NewManager(r.cfg.AuthFile, r.client, tokenURL)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}

	p := newProxy(accProxyCfg, authMgr, r.client, r.logger)
	fetcher := &quota.Fetcher{
		Client:        r.client,
		Endpoint:      r.cfg.UsageURL,
		UserAgent:     r.cfg.UserAgent,
		ClientVersion: r.cfg.ClientVersion,
	}

	namespace := accountNamespace(accCfg)
	candidate := &accountRuntime{
		id:        accCfg.ID,
		label:     accCfg.Label,
		enabled:   accCfg.Enabled,
		proxy:     p,
		auth:      authMgr,
		quota:     fetcher,
		namespace: namespace,
	}

	if !accCfg.Enabled || authMgr == nil || !r.isManagementEnabled() {
		return candidate, nil
	}

	switch {
	case r.status != nil:
		candidate.status = &statusAdapter{svc: r.status, namespace: namespace}
	case previous != nil && previous.status != nil && previous.namespace == namespace:
		candidate.status = previous.status
	default:
		statusSvc, err := status.NewAccountService(r.ctx, status.AccountOptions{
			Dir:        r.cfg.HistoryDir,
			Namespace:  namespace,
			MaxSamples: r.cfg.HistoryMaxSamples,
			Interval:   r.cfg.PollInterval,
			Fetch: func(ctx context.Context) (quota.Snapshot, error) {
				creds, err := authMgr.Current(ctx)
				if err != nil {
					return quota.Snapshot{}, err
				}
				return fetcher.Fetch(ctx, creds)
			},
		})
		if err != nil {
			return nil, err
		}
		candidate.status = statusSvc
	}

	return candidate, nil
}

func (r *Runtime) persistTokenRefresh(accID string) func(oauth.Tokens) error {
	return func(tokens oauth.Tokens) error {
		return r.persist(r.configPath, func(accounts *[]config.Account) error {
			for i := range *accounts {
				if (*accounts)[i].ID == accID {
					(*accounts)[i].AccessToken = tokens.AccessToken
					if tokens.RefreshToken != "" {
						(*accounts)[i].RefreshToken = tokens.RefreshToken
					}
					if tokens.IDToken != "" {
						(*accounts)[i].IDToken = tokens.IDToken
					}
					return nil
				}
			}
			return fmt.Errorf("account %q not found", accID)
		})
	}
}

func (r *Runtime) reloadRegistryLocked() error {
	diskAccounts, err := r.readDiskAccounts()
	if err != nil {
		return err
	}
	if len(diskAccounts) == 0 {
		diskAccounts = r.cfg.Accounts
	}
	if len(diskAccounts) == 0 {
		diskAccounts = []config.Account{
			{
				ID:      "default",
				Label:   "Default",
				Enabled: true,
			},
		}
	}

	prevReg := r.registry.Load()
	newAccounts := make(map[string]*accountRuntime, len(diskAccounts))
	bindings := make(map[string]apikey.Binding)

	for _, da := range diskAccounts {
		var prev *accountRuntime
		if prevReg != nil {
			prev = prevReg.accounts[da.ID]
		}
		acc, err := r.prepareAccount(da, prev)
		if err != nil {
			return err
		}
		newAccounts[da.ID] = acc

		for _, k := range da.Keys {
			bindings[k.Hash] = apikey.Binding{AccountID: da.ID, KeyID: k.ID}
		}
	}

	if r.cfg.APIKey != "" {
		hash := apikey.Hash(r.cfg.APIKey)
		if _, exists := bindings[hash]; !exists {
			bindings[hash] = apikey.Binding{AccountID: "default", KeyID: "key_env"}
		}
	}

	keysReg, err := apikey.NewRegistry(bindings)
	if err != nil {
		return err
	}

	newReg := &runtimeRegistry{
		accounts: newAccounts,
		keys:     keysReg,
	}
	if prevReg != nil {
		prevReg.lastUsed.Range(func(key, value any) bool {
			if keyStr, ok := key.(string); ok && keysReg.HasKeyID(keyStr) {
				newReg.lastUsed.Store(keyStr, value)
			}
			return true
		})
	}

	r.registry.Store(newReg)

	// Close old status services that were removed, disabled, or replaced
	if prevReg != nil {
		for id, oldAcc := range prevReg.accounts {
			newAcc, stillExists := newAccounts[id]
			if (!stillExists || !newAcc.enabled || newAcc.status != oldAcc.status) && oldAcc.status != nil {
				_ = oldAcc.status.Close()
			}
		}
	}

	// Update backward-compatible aliases
	if def, ok := newAccounts["default"]; ok {
		r.setAuth(def.auth)
		r.inference = def.proxy
	} else {
		for _, acc := range newAccounts {
			r.setAuth(acc.auth)
			r.inference = acc.proxy
			break
		}
	}

	return nil
}

func (r *Runtime) activate(
	ctx context.Context,
	opts oauth.SessionOptions,
	tokens oauth.Tokens,
) (oauth.Profile, oauth.AccountConfigResult, error) {
	identity, err := tokens.Profile()
	if err != nil {
		return oauth.Profile{}, oauth.AccountConfigResult{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	diskAccounts, err := r.readDiskAccounts()
	if err != nil {
		return oauth.Profile{}, oauth.AccountConfigResult{}, err
	}
	if len(diskAccounts) == 0 {
		diskAccounts = r.cfg.Accounts
	}

	targetID := opts.AccountID
	existingIDs := make([]string, 0, len(diskAccounts))
	for _, da := range diskAccounts {
		existingIDs = append(existingIDs, da.ID)
	}

	if targetID != "" {
		for _, da := range diskAccounts {
			if da.AccountID == identity.AccountID && da.ID != targetID {
				return oauth.Profile{}, oauth.AccountConfigResult{}, oauth.ErrAccountAlreadyLinked
			}
		}
		for _, da := range diskAccounts {
			if da.ID == targetID && da.AccountID != "" && da.AccountID != identity.AccountID {
				if !opts.Replace {
					return oauth.Profile{}, oauth.AccountConfigResult{}, oauth.ErrAccountIdentityMismatch
				}
			}
		}
	} else {
		matched := false
		for _, da := range diskAccounts {
			if da.AccountID == identity.AccountID {
				targetID = da.ID
				matched = true
				break
			}
		}
		if !matched {
			if len(diskAccounts) == 0 || (len(diskAccounts) == 1 && diskAccounts[0].ID == "default") {
				targetID = "default"
			} else {
				targetID = config.DeriveAccountID(identity.Email, existingIDs)
			}
		}
	}

	if r.isEnvAccount(targetID) {
		return oauth.Profile{}, oauth.AccountConfigResult{}, fmt.Errorf("account %q is managed by environment variables and cannot be replaced", targetID)
	}

	var mintedKeyID, mintedSecret string
	var newKey *config.AccountKey
	if opts.IssueKey != "" {
		mintedSecret, err = apikey.Generate()
		if err != nil {
			return oauth.Profile{}, oauth.AccountConfigResult{}, fmt.Errorf("generate key secret: %w", err)
		}
		mintedKeyID, err = apikey.GenerateID()
		if err != nil {
			return oauth.Profile{}, oauth.AccountConfigResult{}, fmt.Errorf("generate key id: %w", err)
		}
		newKey = &config.AccountKey{
			ID:        mintedKeyID,
			Label:     opts.IssueKey,
			Hash:      apikey.Hash(mintedSecret),
			Last4:     apikey.Last4(mintedSecret),
			CreatedAt: time.Now().UTC(),
		}
	}

	creds := config.Credentials{
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		IDToken:      tokens.IDToken,
		AccountID:    identity.AccountID,
		AccountEmail: identity.Email,
		AccountName:  identity.Name,
		PlanType:     identity.PlanType,
	}

	// Prepare account before persisting
	if r.status != nil {
		if err := r.status.Prepare(identity.Scope()); err != nil {
			return oauth.Profile{}, oauth.AccountConfigResult{}, errors.New("cannot prepare account quota status")
		}
	}

	writeErr := r.persist(r.configPath, func(accounts *[]config.Account) error {
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

	var committed *storage.CommitError
	if writeErr != nil && !errors.As(writeErr, &committed) {
		return oauth.Profile{}, oauth.AccountConfigResult{}, writeErr
	}

	// Also update in-memory cfg.Accounts
	found := false
	for i := range r.cfg.Accounts {
		if r.cfg.Accounts[i].ID == targetID {
			r.cfg.Accounts[i].ApplyCredentials(creds)
			if newKey != nil {
				r.cfg.Accounts[i].Keys = append(r.cfg.Accounts[i].Keys, *newKey)
			}
			found = true
			break
		}
	}
	if !found {
		var keys []config.AccountKey
		if newKey != nil {
			keys = append(keys, *newKey)
		}
		acc := config.Account{ID: targetID, Label: targetID, Enabled: true, Keys: keys}
		acc.ApplyCredentials(creds)
		r.cfg.Accounts = append(r.cfg.Accounts, acc)
	}

	if curAuth := r.getAuth(); curAuth != nil && r.cfg.AuthFile != "" {
		_, _ = curAuth.Replace(ctx, tokens)
	}

	if err := r.reloadRegistryLocked(); err != nil {
		return oauth.Profile{}, oauth.AccountConfigResult{}, err
	}

	if r.status != nil {
		r.status.TriggerPoll()
	}

	res := oauth.AccountConfigResult{
		AccountID: targetID,
		KeyID:     mintedKeyID,
		KeySecret: mintedSecret,
	}

	return identity, res, writeErr
}

func (r *Runtime) handleAdmin(w http.ResponseWriter, req *http.Request) {
	presented := apikey.Extract(req, false)
	if r.adminKeyHash == "" && r.adminAPIKey == "" {
		httputil.RejectUnreadBody(w, req)
		writeAdminError(w, http.StatusServiceUnavailable, "ADMIN_API_KEY must be configured on the server to use admin routes")
		return
	}

	authorized := false
	if r.adminKeyHash != "" && apikey.Match(presented, r.adminKeyHash) {
		authorized = true
	} else if r.adminAPIKey != "" && apikey.Match(presented, r.adminAPIKey) {
		authorized = true
	}

	if !authorized {
		httputil.RejectUnreadBody(w, req)
		writeAdminError(w, http.StatusUnauthorized, "invalid admin API key")
		return
	}

	r.adminHandler.ServeHTTP(w, req)
}

func (r *Runtime) hasAnyCredentialConfigured() bool {
	reg := r.registry.Load()
	if reg != nil {
		for _, acc := range reg.accounts {
			if acc.auth != nil {
				if prof, err := acc.auth.Profile(); err == nil && prof.CredentialConfigured && prof.AccountID != "" {
					return true
				}
			}
		}
	}
	if r.cfg.AuthFile != "" {
		info, err := os.Stat(r.cfg.AuthFile)
		if err == nil && info.Mode().IsRegular() && info.Size() > 0 {
			if doc, err := oauth.NewManager(r.cfg.AuthFile, r.client, oauth.DefaultTokenURL); err == nil {
				if prof, err := doc.Profile(); err == nil && prof.CredentialConfigured && prof.AccountID != "" {
					return true
				}
			}
		}
	}
	return false
}

// Close gracefully stops the runtime, background workers, and quota polling.
func (r *Runtime) Close() error {
	r.closeOnce.Do(func() {
		if r.cancel != nil {
			r.cancel()
		}
		if r.login != nil {
			r.login.Close()
		}
		if r.status != nil {
			r.closeErr = r.status.Close()
		}
		reg := r.registry.Load()
		if reg != nil {
			for _, acc := range reg.accounts {
				if acc.status != nil {
					_ = acc.status.Close()
				}
			}
		}
		if r.auth != nil {
			r.auth.WaitForRefresh()
		}
		if r.client != nil {
			r.client.CloseIdleConnections()
		}
		if r.lock != nil {
			r.closeErr = errors.Join(r.closeErr, r.lock.Close())
		}
	})
	return r.closeErr
}
