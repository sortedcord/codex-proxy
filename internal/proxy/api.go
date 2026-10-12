package proxy

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sortedcord/codex-proxy/internal/httputil"
	"github.com/sortedcord/codex-proxy/internal/oauth"
	"github.com/sortedcord/codex-proxy/internal/quota"
	"github.com/sortedcord/codex-proxy/internal/status"
	"github.com/sortedcord/codex-proxy/internal/storage"
)

func (r *Runtime) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if request.URL.Path == "/health" {
		if !requireMethod(w, request, http.MethodGet) {
			return
		}
		// Liveness must not wait for a refresh mutex or contact upstream.
		info, err := os.Stat(r.cfg.AuthFile)
		configured := err == nil && info.Mode().IsRegular()
		r.writeManagementJSON(w, http.StatusOK, map[string]any{"status": "ok", "credential_configured": configured})
		return
	}
	management := strings.HasPrefix(request.URL.Path, "/status/") || strings.HasPrefix(request.URL.Path, "/config/")
	if !management {
		r.inference.ServeHTTP(w, request)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.cfg.ManagementKey == "" {
		errManagementDisabled.reject(w, request)
		return
	}
	if !httputil.BearerMatches(request, r.cfg.ManagementKey) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		errInvalidManagementKey.reject(w, request)
		return
	}
	switch request.URL.Path {
	case "/config/login":
		r.handleLogin(w, request)
	case "/status/account", "/status/limit", "/status/usage":
		r.handleStatus(w, request)
	default:
		httputil.RejectUnreadBody(w, request)
		writeError(
			w,
			http.StatusNotFound,
			"not_found",
			"unknown management endpoint",
		)
	}
}

func (r *Runtime) handleStatus(w http.ResponseWriter, request *http.Request) {
	if !requireMethod(w, request, http.MethodGet) {
		return
	}
	identity, err := r.auth.Profile()
	if err != nil {
		httputil.RejectUnreadBody(w, request)
		writeError(
			w,
			http.StatusServiceUnavailable,
			"credentials_unavailable",
			"Codex credentials are not configured or available",
		)
		return
	}
	snapshot, available, polling := r.status.Latest(identity.Scope())
	switch request.URL.Path {
	case "/status/account":
		r.handleStatusAccount(w, identity, snapshot, available, polling)
	case "/status/limit":
		r.handleStatusLimit(w, snapshot, available, polling)
	case "/status/usage":
		r.handleStatusUsage(w, request, identity, polling)
	}
}

func (r *Runtime) handleStatusAccount(
	w http.ResponseWriter,
	identity oauth.Profile,
	snapshot quota.Snapshot,
	available bool,
	polling status.PollInfo,
) {
	if available && snapshot.PlanType != "" {
		identity.PlanType = snapshot.PlanType
		identity.PlanSource = "wham/usage"
	}
	r.writeManagementJSON(w, http.StatusOK, struct {
		oauth.Profile
		QuotaAvailable bool `json:"quota_available"`
		QuotaStale     bool `json:"quota_stale"`
	}{
		Profile:        identity,
		QuotaAvailable: available,
		QuotaStale:     !available || r.status.Stale(snapshot, polling),
	})
}

func (r *Runtime) handleStatusLimit(
	w http.ResponseWriter,
	snapshot quota.Snapshot,
	available bool,
	polling status.PollInfo,
) {
	if !available {
		r.writeManagementJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]string{
				"type":    "server_error",
				"code":    "quota_unavailable",
				"message": "quota has not been observed for the active account",
			},
			"polling": polling,
		})
		return
	}
	r.writeManagementJSON(w, http.StatusOK, struct {
		quota.Snapshot
		Polling status.PollInfo `json:"polling"`
		Stale   bool            `json:"stale"`
	}{
		Snapshot: snapshot,
		Polling:  polling,
		Stale:    r.status.Stale(snapshot, polling),
	})
}

func (r *Runtime) handleStatusUsage(
	w http.ResponseWriter,
	request *http.Request,
	identity oauth.Profile,
	polling status.PollInfo,
) {
	query, err := status.ParseQuery(request.URL.RawQuery)
	if err != nil {
		writeError(
			w,
			http.StatusBadRequest,
			"invalid_request",
			err.Error(),
		)
		return
	}
	result, err := r.status.Query(identity.Scope(), query)
	if err != nil {
		writeError(
			w,
			http.StatusServiceUnavailable,
			"history_unavailable",
			"quota history is unavailable",
		)
		return
	}
	r.writeManagementJSON(w, http.StatusOK, struct {
		status.Result
		Polling status.PollInfo `json:"polling"`
	}{
		Result:  result,
		Polling: polling,
	})
}

func (r *Runtime) handleLogin(w http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodGet {
		info, err := r.login.Begin(request.Context())
		if err != nil {
			r.loginError(w, err)
			return
		}
		r.writeManagementJSON(w, http.StatusOK, info)
		return
	}
	if request.Method != http.MethodPost {
		httputil.RejectUnreadBody(w, request)
		w.Header().Set("Allow", "GET, POST")
		writeError(
			w,
			http.StatusMethodNotAllowed,
			"method_not_allowed",
			"use GET or POST",
		)
		return
	}
	if !validateJSONRequest(w, request) {
		return
	}
	input := struct {
		Code  string `json:"code"`
		State string `json:"state"`
		URL   string `json:"url"`
	}{}
	decoder := json.NewDecoder(http.MaxBytesReader(w, request.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		httputil.RejectUnreadBody(w, request)
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(
				w,
				http.StatusRequestEntityTooLarge,
				"request_too_large",
				"login body exceeds 64 KiB",
			)
			return
		}
		writeError(
			w,
			http.StatusBadRequest,
			"invalid_request",
			"invalid login JSON",
		)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		httputil.RejectUnreadBody(w, request)
		writeError(
			w,
			http.StatusBadRequest,
			"invalid_request",
			"send one JSON object",
		)
		return
	}
	var identity oauth.Profile
	var err error
	if input.URL != "" {
		if input.Code != "" || input.State != "" {
			writeError(
				w,
				http.StatusBadRequest,
				"invalid_request",
				"use URL or code/state, not both",
			)
			return
		}
		identity, err = r.login.CompleteURL(request.Context(), input.URL)
	} else {
		identity, err = r.login.Complete(request.Context(), input.Code, input.State)
	}
	if err != nil {
		var committed *storage.CommitError
		if errors.As(err, &committed) {
			r.writeManagementJSON(w, http.StatusInternalServerError, map[string]any{
				"credential_configured": true,
				"durability_uncertain":  true,
				"error":                 "account activated but credential durability is uncertain",
			})
			return
		}
		r.loginError(w, err)
		return
	}
	r.writeManagementJSON(w, http.StatusOK, struct {
		oauth.Profile
		Status string `json:"status"`
	}{Profile: identity, Status: "configured"})
}

func (r *Runtime) loginError(w http.ResponseWriter, err error) {
	code := http.StatusBadGateway
	switch {
	case errors.Is(err, oauth.ErrInvalidState):
		code = http.StatusBadRequest
	case errors.Is(err, oauth.ErrLoginInProgress), errors.Is(err, oauth.ErrSessionUsed):
		code = http.StatusConflict
	case errors.Is(err, oauth.ErrSessionExpired):
		code = http.StatusGone
	case errors.Is(err, oauth.ErrSessionClosed):
		code = http.StatusServiceUnavailable
	case errors.Is(err, oauth.ErrActivation):
		code = http.StatusInternalServerError
	}
	writeError(
		w,
		code,
		"login_failed",
		"login failed or is unavailable; start a new attempt if needed",
	)
}

func (r *Runtime) writeManagementJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(httputil.WriteDeadline))
	w.WriteHeader(code)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}
