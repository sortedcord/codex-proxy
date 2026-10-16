package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/sortedcord/codex-proxy/internal/apikey"
	"github.com/sortedcord/codex-proxy/internal/config"
	"github.com/sortedcord/codex-proxy/internal/oauth"
	"github.com/sortedcord/codex-proxy/internal/quota"
	"github.com/sortedcord/codex-proxy/internal/status"
	"github.com/sortedcord/codex-proxy/internal/storage"
	"github.com/terastal-hq/proxylib/httpx"
)

func writeAdminJSON(w http.ResponseWriter, code int, data any) {
	httpx.WriteJSON(w, code, data)
}

func writeAdminError(w http.ResponseWriter, code int, msg string) {
	httpx.WriteError(w, code, msg)
}

func writePersistError(w http.ResponseWriter, err error) {
	if errors.Is(err, config.ErrUnloadableConfig) {
		writeAdminError(w, http.StatusConflict, err.Error())
		return
	}
	writeAdminError(w, http.StatusInternalServerError, err.Error())
}

func (r *Runtime) setupAdminMux() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /admin/accounts", r.handleAdminListAccounts)
	mux.HandleFunc("GET /admin/accounts/{id}", r.handleAdminGetAccount)
	mux.HandleFunc("PATCH /admin/accounts/{id}", r.handleAdminPatchAccount)
	mux.HandleFunc("DELETE /admin/accounts/{id}", r.handleAdminDeleteAccount)

	mux.HandleFunc("POST /admin/login-sessions", r.handleAdminCreateLoginSession)
	mux.HandleFunc("GET /admin/login-sessions/{id}", r.handleAdminGetLoginSession)
	mux.HandleFunc("POST /admin/login-sessions/{id}/complete", r.handleAdminCompleteLoginSession)

	mux.HandleFunc("GET /admin/accounts/{id}/keys", r.handleAdminListKeys)
	mux.HandleFunc("POST /admin/accounts/{id}/keys", r.handleAdminCreateKey)
	mux.HandleFunc("DELETE /admin/accounts/{id}/keys/{keyId}", r.handleAdminDeleteKey)

	mux.HandleFunc("GET /admin/accounts/{id}/quota", r.handleAdminAccountQuota)
	mux.HandleFunc("GET /admin/accounts/{id}/usage", r.handleAdminAccountUsage)
	mux.HandleFunc("GET /admin/quota", r.handleAdminQuotaOverview)

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, req)
	})
}

func (r *Runtime) handleAdminListAccounts(w http.ResponseWriter, req *http.Request) {
	reg := r.registry.Load()
	if reg == nil {
		writeAdminError(w, http.StatusServiceUnavailable, "runtime is not started")
		return
	}

	diskAccounts, err := r.readDiskAccounts()
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, err.Error())
		return
	}

	type accountSummary struct {
		ID                   string           `json:"id"`
		Label                string           `json:"label,omitempty"`
		Email                string           `json:"email,omitempty"`
		Name                 string           `json:"name,omitempty"`
		Enabled              bool             `json:"enabled"`
		CredentialConfigured bool             `json:"credential_configured"`
		KeyCount             int              `json:"key_count"`
		Quota                *quota.Snapshot  `json:"quota,omitempty"`
		Polling              *status.PollInfo `json:"polling,omitempty"`
		Stale                bool             `json:"stale"`
	}

	summaries := make([]accountSummary, 0, len(diskAccounts))
	for _, da := range diskAccounts {
		summary := accountSummary{
			ID:                   da.ID,
			Label:                da.Label,
			Email:                da.AccountEmail,
			Name:                 da.AccountName,
			Enabled:              da.Enabled,
			CredentialConfigured: da.HasCredentials(),
			KeyCount:             len(da.Keys),
		}
		if acc, ok := reg.accounts[da.ID]; ok && acc.status != nil {
			snapshot, avail, pollInfo := acc.status.Latest()
			if avail {
				summary.Quota = &snapshot
				summary.Stale = acc.status.Stale(snapshot, pollInfo)
			}
			summary.Polling = &pollInfo
		}
		summaries = append(summaries, summary)
	}

	writeAdminJSON(w, http.StatusOK, summaries)
}

func (r *Runtime) handleAdminGetAccount(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	diskAccounts, err := r.readDiskAccounts()
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var target *config.Account
	for i := range diskAccounts {
		if diskAccounts[i].ID == id {
			target = &diskAccounts[i]
			break
		}
	}
	if target == nil {
		writeAdminError(w, http.StatusNotFound, fmt.Sprintf("account %q not found", id))
		return
	}

	reg := r.registry.Load()
	resp := map[string]any{
		"id":                    target.ID,
		"label":                 target.Label,
		"email":                 target.AccountEmail,
		"name":                  target.AccountName,
		"enabled":               target.Enabled,
		"credential_configured": target.HasCredentials(),
		"key_count":             len(target.Keys),
		"plan_type":             target.PlanType,
	}

	if reg != nil {
		if acc, ok := reg.accounts[id]; ok && acc.auth != nil {
			if prof, err := acc.auth.Profile(); err == nil {
				if prof.Email != "" {
					resp["email"] = prof.Email
				}
				if prof.Name != "" {
					resp["name"] = prof.Name
				}
				if prof.PlanType != "" {
					resp["plan_type"] = prof.PlanType
				}
				if prof.PlanSource != "" {
					resp["plan_source"] = prof.PlanSource
				}
			}
		}
	}

	writeAdminJSON(w, http.StatusOK, resp)
}

func (r *Runtime) handleAdminPatchAccount(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if r.isEnvAccount(id) {
		writeAdminError(w, http.StatusConflict, fmt.Sprintf("account %q is managed by environment variables and cannot be modified", id))
		return
	}
	var payload struct {
		Label   *string `json:"label"`
		Enabled *bool   `json:"enabled"`
	}
	req.Body = http.MaxBytesReader(w, req.Body, 64<<10)
	decoder := json.NewDecoder(req.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid request JSON")
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	var updatedAccount config.Account
	var notFound bool
	err := r.persist(r.configPath, func(accounts *[]config.Account) error {
		for i := range *accounts {
			if (*accounts)[i].ID == id {
				if payload.Label != nil {
					(*accounts)[i].Label = strings.TrimSpace(*payload.Label)
				}
				if payload.Enabled != nil {
					(*accounts)[i].Enabled = *payload.Enabled
				}
				updatedAccount = (*accounts)[i]
				return nil
			}
		}
		notFound = true
		return fmt.Errorf("account %q not found", id)
	})

	if notFound {
		writeAdminError(w, http.StatusNotFound, fmt.Sprintf("account %q not found", id))
		return
	}
	if err != nil {
		writePersistError(w, err)
		return
	}

	if err := r.reloadRegistryLocked(); err != nil {
		writeAdminError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeAdminJSON(w, http.StatusOK, map[string]any{
		"id":      updatedAccount.ID,
		"label":   updatedAccount.Label,
		"enabled": updatedAccount.Enabled,
	})
}

func (r *Runtime) handleAdminDeleteAccount(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if r.isEnvAccount(id) {
		writeAdminError(w, http.StatusConflict, fmt.Sprintf("account %q is managed by environment variables and cannot be deleted", id))
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	var notFound bool
	err := r.persist(r.configPath, func(accounts *[]config.Account) error {
		filtered := make([]config.Account, 0, len(*accounts))
		for _, acc := range *accounts {
			if acc.ID == id {
				continue
			}
			filtered = append(filtered, acc)
		}
		if len(filtered) == len(*accounts) {
			notFound = true
			return fmt.Errorf("account %q not found", id)
		}
		*accounts = filtered
		return nil
	})

	if notFound {
		writeAdminError(w, http.StatusNotFound, fmt.Sprintf("account %q not found", id))
		return
	}
	if err != nil {
		writePersistError(w, err)
		return
	}

	if err := r.reloadRegistryLocked(); err != nil {
		writeAdminError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (r *Runtime) handleAdminCreateLoginSession(w http.ResponseWriter, req *http.Request) {
	var payload struct {
		Account  string `json:"account"`
		Replace  bool   `json:"replace"`
		IssueKey string `json:"issue_key"`
	}
	if req.Body != nil && req.Body != http.NoBody {
		req.Body = http.MaxBytesReader(w, req.Body, 64<<10)
		decoder := json.NewDecoder(req.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&payload); err != nil && !errors.Is(err, io.EOF) {
			writeAdminError(w, http.StatusBadRequest, "invalid request JSON")
			return
		}
	}

	opts := oauth.SessionOptions{
		AccountID: strings.TrimSpace(payload.Account),
		Replace:   payload.Replace,
		IssueKey:  strings.TrimSpace(payload.IssueKey),
	}
	if opts.AccountID != "" {
		if r.isEnvAccount(opts.AccountID) {
			writeAdminError(w, http.StatusConflict, fmt.Sprintf("account %q is managed by environment variables and cannot be targeted for login", opts.AccountID))
			return
		}
		if err := config.ValidateAccountID(opts.AccountID); err != nil {
			writeAdminError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	info, err := r.login.BeginWithOptions(req.Context(), opts)
	if err != nil {
		writeLoginError(w, err)
		return
	}

	writeAdminJSON(w, http.StatusOK, map[string]any{
		"id":         info.ID,
		"login_url":  info.AuthURL,
		"expires_at": info.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

func (r *Runtime) handleAdminGetLoginSession(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	sessStatus, found := r.login.GetSession(id)
	if !found {
		writeAdminError(w, http.StatusNotFound, fmt.Sprintf("login session %q not found", id))
		return
	}

	resp := map[string]any{
		"id":         sessStatus.ID,
		"status":     sessStatus.Status,
		"expires_at": sessStatus.ExpiresAt.UTC().Format(time.RFC3339),
	}
	if sessStatus.AuthURL != "" {
		resp["login_url"] = sessStatus.AuthURL
	}
	if sessStatus.AccountID != "" {
		resp["account_id"] = sessStatus.AccountID
	}
	if sessStatus.Email != "" {
		resp["email"] = sessStatus.Email
		resp["name"] = sessStatus.Name
	}
	if sessStatus.Key != nil {
		resp["key"] = sessStatus.Key
	}
	if sessStatus.Error != "" {
		resp["error"] = sessStatus.Error
	}

	writeAdminJSON(w, http.StatusOK, resp)
}

func (r *Runtime) handleAdminCompleteLoginSession(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	var submission struct {
		Code  string `json:"code"`
		State string `json:"state"`
		URL   string `json:"url"`
	}
	req.Body = http.MaxBytesReader(w, req.Body, 64<<10)
	decoder := json.NewDecoder(req.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&submission); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid login JSON")
		return
	}

	code, state := strings.TrimSpace(submission.Code), strings.TrimSpace(submission.State)
	if submission.URL != "" {
		if code != "" || state != "" {
			writeAdminError(w, http.StatusBadRequest, "provide code/state or callback URL, not both")
			return
		}
		callback, err := url.Parse(submission.URL)
		if err != nil || callback.Scheme != "http" || callback.Hostname() != "127.0.0.1" || callback.User != nil {
			writeAdminError(w, http.StatusBadRequest, "invalid loopback callback URL")
			return
		}
		code, state = callback.Query().Get("code"), callback.Query().Get("state")
	}
	if code == "" || state == "" {
		writeAdminError(w, http.StatusBadRequest, "both code and state are required")
		return
	}

	user, res, err := r.login.CompleteWithSessionID(req.Context(), id, code, state)
	var commit *storage.CommitError
	if err != nil && !errors.As(err, &commit) {
		writeLoginError(w, err)
		return
	}
	r.login.ConsumeSessionKey(id)

	resp := map[string]any{
		"status":     "configured",
		"account_id": res.AccountID,
		"email":      user.Email,
		"name":       user.Name,
	}
	if res.KeySecret != "" {
		resp["key"] = map[string]string{
			"id":     res.KeyID,
			"secret": res.KeySecret,
		}
	}
	if errors.As(err, &commit) {
		resp["credential_configured"] = true
		resp["durability_uncertain"] = true
		resp["error"] = "account credentials saved to disk, but runtime activation had a warning"
		writeAdminJSON(w, http.StatusInternalServerError, resp)
		return
	}

	writeAdminJSON(w, http.StatusOK, resp)
}

func (r *Runtime) handleAdminListKeys(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	diskAccounts, err := r.readDiskAccounts()
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var target *config.Account
	for i := range diskAccounts {
		if diskAccounts[i].ID == id {
			target = &diskAccounts[i]
			break
		}
	}
	if target == nil {
		writeAdminError(w, http.StatusNotFound, fmt.Sprintf("account %q not found", id))
		return
	}

	reg := r.registry.Load()
	keys := make([]apikey.KeyMetadata, 0, len(target.Keys))
	for _, k := range target.Keys {
		meta := apikey.KeyMetadata{
			ID:        k.ID,
			Label:     k.Label,
			Last4:     k.Last4,
			CreatedAt: k.CreatedAt,
		}
		if reg != nil {
			meta.LastUsed = reg.getKeyLastUsed(k.ID)
		}
		keys = append(keys, meta)
	}

	writeAdminJSON(w, http.StatusOK, keys)
}

func (r *Runtime) handleAdminCreateKey(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if r.isEnvAccount(id) {
		writeAdminError(w, http.StatusConflict, fmt.Sprintf("account %q is managed by environment variables and cannot have keys modified", id))
		return
	}
	var payload struct {
		Label string `json:"label"`
	}
	if req.Body != nil && req.Body != http.NoBody {
		req.Body = http.MaxBytesReader(w, req.Body, 64<<10)
		decoder := json.NewDecoder(req.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&payload); err != nil && !errors.Is(err, io.EOF) {
			writeAdminError(w, http.StatusBadRequest, "invalid request JSON")
			return
		}
	}

	rawSecret, err := apikey.Generate()
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "generate key secret")
		return
	}
	keyID, err := apikey.GenerateID()
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "generate key id")
		return
	}

	keyObj := config.AccountKey{
		ID:        keyID,
		Label:     strings.TrimSpace(payload.Label),
		Hash:      apikey.Hash(rawSecret),
		Last4:     apikey.Last4(rawSecret),
		CreatedAt: time.Now().UTC(),
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	var notFound bool
	err = r.persist(r.configPath, func(accounts *[]config.Account) error {
		for i := range *accounts {
			if (*accounts)[i].ID == id {
				(*accounts)[i].Keys = append((*accounts)[i].Keys, keyObj)
				return nil
			}
		}
		notFound = true
		return fmt.Errorf("account %q not found", id)
	})

	if notFound {
		writeAdminError(w, http.StatusNotFound, fmt.Sprintf("account %q not found", id))
		return
	}
	if err != nil {
		writePersistError(w, err)
		return
	}

	if err := r.reloadRegistryLocked(); err != nil {
		writeAdminError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeAdminJSON(w, http.StatusCreated, map[string]string{
		"id":    keyID,
		"label": keyObj.Label,
		"key":   rawSecret,
	})
}

func (r *Runtime) handleAdminDeleteKey(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	keyID := req.PathValue("keyId")
	if r.isEnvAccount(id) {
		writeAdminError(w, http.StatusConflict, fmt.Sprintf("account %q is managed by environment variables and cannot have keys modified", id))
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	var notFoundAccount bool
	var notFoundKey bool

	err := r.persist(r.configPath, func(accounts *[]config.Account) error {
		for i := range *accounts {
			if (*accounts)[i].ID == id {
				filtered := make([]config.AccountKey, 0, len((*accounts)[i].Keys))
				for _, k := range (*accounts)[i].Keys {
					if k.ID == keyID {
						continue
					}
					filtered = append(filtered, k)
				}
				if len(filtered) == len((*accounts)[i].Keys) {
					notFoundKey = true
					return fmt.Errorf("key %q not found", keyID)
				}
				(*accounts)[i].Keys = filtered
				return nil
			}
		}
		notFoundAccount = true
		return fmt.Errorf("account %q not found", id)
	})

	if notFoundAccount {
		writeAdminError(w, http.StatusNotFound, fmt.Sprintf("account %q not found", id))
		return
	}
	if notFoundKey {
		writeAdminError(w, http.StatusNotFound, fmt.Sprintf("key %q not found", keyID))
		return
	}
	if err != nil {
		writePersistError(w, err)
		return
	}

	if err := r.reloadRegistryLocked(); err != nil {
		writeAdminError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (r *Runtime) handleAdminAccountQuota(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	reg := r.registry.Load()
	if reg == nil {
		writeAdminError(w, http.StatusServiceUnavailable, "runtime is not started")
		return
	}
	acc, ok := reg.accounts[id]
	if !ok {
		writeAdminError(w, http.StatusNotFound, fmt.Sprintf("account %q not found", id))
		return
	}
	if acc.status == nil {
		writeAdminError(w, http.StatusServiceUnavailable, "quota service not configured for account")
		return
	}
	subReq := req.Clone(req.Context())
	subReq.URL.Path = "/status/limit"
	acc.status.ServeHTTP(w, subReq)
}

func (r *Runtime) handleAdminAccountUsage(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	reg := r.registry.Load()
	if reg == nil {
		writeAdminError(w, http.StatusServiceUnavailable, "runtime is not started")
		return
	}
	acc, ok := reg.accounts[id]
	if !ok {
		writeAdminError(w, http.StatusNotFound, fmt.Sprintf("account %q not found", id))
		return
	}
	if acc.status == nil {
		writeAdminError(w, http.StatusServiceUnavailable, "quota service not configured for account")
		return
	}
	subReq := req.Clone(req.Context())
	subReq.URL.Path = "/status/usage"
	acc.status.ServeHTTP(w, subReq)
}

func (r *Runtime) handleAdminQuotaOverview(w http.ResponseWriter, req *http.Request) {
	reg := r.registry.Load()
	if reg == nil {
		writeAdminError(w, http.StatusServiceUnavailable, "runtime is not started")
		return
	}

	type accountQuotaEntry struct {
		ID         string                 `json:"id"`
		ObservedAt *time.Time             `json:"observed_at,omitempty"`
		Pools      map[string]quota.Limit `json:"pools,omitempty"`
		PlanType   string                 `json:"plan_type,omitempty"`
		Stale      bool                   `json:"stale"`
		Polling    status.PollInfo        `json:"polling"`
	}

	results := make([]accountQuotaEntry, 0)
	accountIDs := make([]string, 0, len(reg.accounts))
	for id := range reg.accounts {
		accountIDs = append(accountIDs, id)
	}
	slices.Sort(accountIDs)

	for _, id := range accountIDs {
		acc := reg.accounts[id]
		if !acc.enabled || acc.status == nil {
			continue
		}
		snap, avail, polling := acc.status.Latest()
		if !avail {
			results = append(results, accountQuotaEntry{
				ID:      id,
				Stale:   true,
				Polling: polling,
			})
			continue
		}
		entry := accountQuotaEntry{
			ID:         id,
			ObservedAt: &snap.ObservedAt,
			Pools:      snap.Pools,
			PlanType:   snap.PlanType,
			Stale:      acc.status.Stale(snap, polling),
			Polling:    polling,
		}
		results = append(results, entry)
	}

	writeAdminJSON(w, http.StatusOK, map[string]any{
		"accounts": results,
	})
}

func (r *Runtime) readDiskAccounts() ([]config.Account, error) {
	return config.ReadDiskAccounts(r.configPath)
}

func writeLoginError(w http.ResponseWriter, err error) {
	if errors.Is(err, config.ErrUnloadableConfig) {
		writeAdminError(w, http.StatusConflict, err.Error())
		return
	}
	var commit *storage.CommitError
	if errors.As(err, &commit) {
		writeAdminJSON(w, http.StatusInternalServerError, map[string]any{
			"error":                 "account activated, but credential durability could not be confirmed",
			"credential_configured": true,
			"durability_uncertain":  true,
		})
		return
	}
	code, message := http.StatusBadGateway, "failed to complete ChatGPT authorization"
	switch {
	case errors.Is(err, oauth.ErrInvalidState):
		code, message = http.StatusBadRequest, "invalid OAuth state"
	case errors.Is(err, oauth.ErrLoginInProgress):
		code, message = http.StatusConflict, "login completion already in progress"
	case errors.Is(err, oauth.ErrSessionUsed):
		code, message = http.StatusConflict, "OAuth session already used"
	case errors.Is(err, oauth.ErrSessionExpired):
		code, message = http.StatusGone, "OAuth session expired"
	case errors.Is(err, oauth.ErrSessionClosed):
		code, message = http.StatusServiceUnavailable, "login is closed"
	case errors.Is(err, oauth.ErrAccountIdentityMismatch):
		code, message = http.StatusConflict, err.Error()
	case errors.Is(err, oauth.ErrAccountAlreadyLinked):
		code, message = http.StatusConflict, err.Error()
	case errors.Is(err, oauth.ErrSessionNotFound):
		code, message = http.StatusNotFound, err.Error()
	case errors.Is(err, context.Canceled):
		code, message = http.StatusServiceUnavailable, "login canceled"
	}
	writeAdminError(w, code, message)
}
