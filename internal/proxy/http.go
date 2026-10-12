package proxy

import (
	"encoding/json"
	"mime"
	"net/http"

	"github.com/sortedcord/codex-proxy/internal/httputil"
)

type apiError struct {
	status  int
	code    string
	message string
}

var (
	errInvalidInferenceKey = apiError{
		status: http.StatusUnauthorized, code: "invalid_api_key", message: "invalid proxy API key",
	}
	errInvalidManagementKey = apiError{
		status: http.StatusUnauthorized, code: "invalid_api_key", message: "invalid management API key",
	}
	errCredentialsMissing = apiError{
		status: http.StatusServiceUnavailable, code: "credentials_unavailable",
		message: "configure Codex credentials to use inference",
	}
	errManagementDisabled = apiError{
		status: http.StatusServiceUnavailable, code: "management_disabled",
		message: "set CODEX_PROXY_MANAGEMENT_KEY to enable management",
	}
	errUnsupportedEncoding = apiError{
		status: http.StatusUnsupportedMediaType, code: "unsupported_encoding",
		message: "send uncompressed JSON",
	}
	errUnsupportedMedia = apiError{
		status: http.StatusUnsupportedMediaType, code: "unsupported_media_type",
		message: "send application/json",
	}
)

func (e apiError) write(w http.ResponseWriter) {
	writeError(w, e.status, e.code, e.message)
}

func (e apiError) reject(w http.ResponseWriter, r *http.Request) {
	httputil.RejectUnreadBody(w, r)
	e.write(w)
}

func validateJSONRequest(w http.ResponseWriter, r *http.Request) bool {
	if encoding := r.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		errUnsupportedEncoding.reject(w, r)
		return false
	}
	if contentType := r.Header.Get("Content-Type"); contentType != "" {
		media, _, err := mime.ParseMediaType(contentType)
		if err != nil || media != "application/json" {
			errUnsupportedMedia.reject(w, r)
			return false
		}
	}
	return true
}

func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	apiError{status: http.StatusMethodNotAllowed, code: "method_not_allowed", message: "use " + method}.reject(w, r)
	return false
}

func writeJSON(w http.ResponseWriter, status int, body []byte) {
	httputil.WriteJSON(w, status, body)
}

type errorDetail struct {
	Type    string  `json:"type"`
	Code    string  `json:"code"`
	Message string  `json:"message"`
	Param   *string `json:"param"`
}

type errorResponse struct {
	Error errorDetail `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	errorType := "invalid_request_error"
	switch {
	case status == http.StatusUnauthorized:
		errorType = "authentication_error"
	case status == http.StatusTooManyRequests:
		errorType = "rate_limit_error"
	case status >= http.StatusInternalServerError:
		errorType = "server_error"
	}
	body, _ := json.Marshal(errorResponse{Error: errorDetail{
		Type: errorType, Code: code, Message: message, Param: nil,
	}})
	writeJSON(w, status, body)
}
