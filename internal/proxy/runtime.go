package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/sortedcord/codex-proxy/internal/config"
	"github.com/sortedcord/codex-proxy/internal/oauth"
	"github.com/sortedcord/codex-proxy/internal/quota"
	"github.com/sortedcord/codex-proxy/internal/status"
	"github.com/sortedcord/codex-proxy/internal/storage"
)

type Runtime struct {
	cfg       config.Config
	inference *proxy
	auth      *oauth.Manager
	client    *http.Client
	status    *status.Service
	login     *oauth.SessionManager
	lock      *storage.Lock
	cancel    context.CancelFunc
	closeOnce sync.Once
	closeErr  error
}

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
	if err := os.MkdirAll(filepath.Dir(cfg.AuthFile), 0700); err != nil {
		return nil, err
	}
	if err := storage.InspectFile(cfg.AuthFile); err != nil {
		return nil, err
	}
	// Canonicalize the parent so equivalent directory aliases share a lock.
	parent, err := filepath.EvalSymlinks(filepath.Dir(cfg.AuthFile))
	if err != nil {
		return nil, err
	}
	cfg.AuthFile = filepath.Join(parent, filepath.Base(cfg.AuthFile))
	lock, err := storage.Acquire(cfg.AuthFile + ".codex-proxy.lock")
	if err != nil {
		return nil, err
	}
	owned, cancel := context.WithCancel(ctx)
	client := newHTTPClient()
	tokenURL := loginOptions.TokenURL
	if tokenURL == "" {
		tokenURL = oauth.DefaultTokenURL
	}
	auth, err := oauth.NewManager(cfg.AuthFile, client, tokenURL)
	if err != nil {
		cancel()
		client.CloseIdleConnections()
		_ = lock.Close()
		return nil, err
	}
	runtime := &Runtime{cfg: cfg, auth: auth, client: client, lock: lock, cancel: cancel}
	runtime.inference = newProxy(
		cfg,
		auth,
		client,
		logger,
	)
	if cfg.ManagementKey == "" {
		return runtime, nil
	}
	fetcher := &quota.Fetcher{
		Client: client, Endpoint: cfg.UsageURL, UserAgent: cfg.UserAgent, ClientVersion: cfg.ClientVersion,
	}
	runtime.status, err = status.NewService(owned, status.Options{
		Dir: cfg.HistoryDir, MaxSamples: cfg.HistoryMaxSamples, Interval: cfg.PollInterval,
		Account: auth.Profile,
		Fetch: func(ctx context.Context) (quota.Snapshot, error) {
			creds, err := auth.Current(ctx)
			if err != nil {
				return quota.Snapshot{}, err
			}
			return fetcher.Fetch(ctx, creds)
		},
	})
	if err != nil {
		cancel()
		auth.WaitForRefresh()
		client.CloseIdleConnections()
		_ = lock.Close()
		return nil, err
	}
	loginOptions.Client = client
	loginOptions.Activate = runtime.activate
	runtime.login, err = oauth.NewSessionManager(owned, loginOptions)
	if err != nil {
		_ = runtime.Close()
		return nil, err
	}
	return runtime, nil
}

func (r *Runtime) activate(ctx context.Context, tokens oauth.Tokens) (oauth.Profile, error) {
	identity, err := tokens.Profile()
	if err != nil {
		return oauth.Profile{}, err
	}
	if err := r.status.Prepare(identity.Scope()); err != nil {
		return oauth.Profile{}, errors.New("cannot prepare account quota history")
	}
	identity, err = r.auth.Replace(ctx, tokens)
	if identity.CredentialConfigured {
		r.status.TriggerPoll()
	}
	return identity, err
}

func (r *Runtime) Close() error {
	r.closeOnce.Do(func() {
		r.cancel()
		if r.login != nil {
			r.login.Close()
		}
		if r.status != nil {
			r.closeErr = r.status.Close()
		}
		r.auth.WaitForRefresh()
		r.client.CloseIdleConnections()
		r.closeErr = errors.Join(r.closeErr, r.lock.Close())
	})
	return r.closeErr
}
