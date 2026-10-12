package proxy

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"

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
		rejectUnreadBody(w, request)
		writeError(
			w,
			http.StatusServiceUnavailable,
			"management_disabled",
			"set CODEX_PROXY_MANAGEMENT_KEY to enable management",
		)
		return
	}
	if !bearerMatches(request, r.cfg.ManagementKey) {
		rejectUnreadBody(w, request)
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(
			w,
			http.StatusUnauthorized,
			"invalid_api_key",
			"invalid management API key",
		)
		return
	}
	switch request.URL.Path {
	case "/config/login":
		r.handleLogin(w, request)
	case "/status/account", "/status/limit", "/status/usage":
		r.handleStatus(w, request)
	default:
		writeError(
			w,
			http.StatusNotFound,
			"not_found",
			"unknown management endpoint",
		)
	}
}

func bearerMatches(request *http.Request, key string) bool {
	parts := strings.SplitN(request.Header.Get("Authorization"), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return false
	}
	expected := sha256.Sum256([]byte(key))
	provided := sha256.Sum256([]byte(parts[1]))
	return subtle.ConstantTimeCompare(expected[:], provided[:]) == 1
}

func rejectUnreadBody(w http.ResponseWriter, request *http.Request) {
	if request.ProtoMajor == 1 && request.Body != nil && request.Body != http.NoBody {
		w.Header().Set("Connection", "close")
		_ = http.NewResponseController(w).SetReadDeadline(time.Now())
	}
}

func (r *Runtime) handleStatus(w http.ResponseWriter, request *http.Request) {
	if !requireMethod(w, request, http.MethodGet) {
		return
	}
	identity, err := r.auth.Profile()
	if err != nil {
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
		if available && snapshot.PlanType != "" {
			identity.PlanType = snapshot.PlanType
			identity.PlanSource = "wham/usage"
		}
		r.writeManagementJSON(w, http.StatusOK, struct {
			oauth.Profile
			QuotaAvailable bool `json:"quota_available"`
			QuotaStale     bool `json:"quota_stale"`
		}{Profile: identity, QuotaAvailable: available, QuotaStale: !available || r.status.Stale(snapshot, polling)})
	case "/status/limit":
		if !available {
			r.writeManagementJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error": map[string]string{"type": "server_error", "code": "quota_unavailable",
					"message": "quota has not been observed for the active account"}, "polling": polling,
			})
			return
		}
		r.writeManagementJSON(w, http.StatusOK, struct {
			quota.Snapshot
			Polling status.PollInfo `json:"polling"`
			Stale   bool            `json:"stale"`
		}{Snapshot: snapshot, Polling: polling, Stale: r.status.Stale(snapshot, polling)})
	case "/status/usage":
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
		}{Result: result, Polling: polling})
	}
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
		w.Header().Set("Allow", "GET, POST")
		writeError(
			w,
			http.StatusMethodNotAllowed,
			"method_not_allowed",
			"use GET or POST",
		)
		return
	}
	if request.Header.Get("Content-Encoding") != "" && request.Header.Get("Content-Encoding") != "identity" {
		writeError(
			w,
			http.StatusUnsupportedMediaType,
			"unsupported_encoding",
			"send uncompressed JSON",
		)
		return
	}
	if contentType := request.Header.Get("Content-Type"); contentType != "" {
		media, _, err := mime.ParseMediaType(contentType)
		if err != nil || media != "application/json" {
			writeError(
				w,
				415,
				"unsupported_media_type",
				"send JSON",
			)
			return
		}
	}
	input := struct {
		Code  string `json:"code"`
		State string `json:"state"`
		URL   string `json:"url"`
	}{}
	decoder := json.NewDecoder(http.MaxBytesReader(w, request.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(
				w,
				413,
				"request_too_large",
				"login body exceeds 64 KiB",
			)
			return
		}
		writeError(
			w,
			400,
			"invalid_request",
			"invalid login JSON",
		)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(
			w,
			400,
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
				400,
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
			r.writeManagementJSON(w, 500, map[string]any{"credential_configured": true, "durability_uncertain": true,
				"error": "account activated but credential durability is uncertain"})
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
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Second))
	w.WriteHeader(code)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}
