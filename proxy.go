package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxRequestBytes = 16 << 20

type proxy struct {
	cfg    config
	auth   *authManager
	client *http.Client
	logger *slog.Logger
}

func newProxy(cfg config, auth *authManager, client *http.Client, logger *slog.Logger) *proxy {
	return &proxy{cfg: cfg, auth: auth, client: client, logger: logger}
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Path == "/healthz" {
		if requireMethod(w, r, http.MethodGet) {
			writeJSON(w, http.StatusOK, []byte(`{"status":"ok"}`))
		}
		return
	}
	if !p.authorized(r) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(
			w,
			http.StatusUnauthorized,
			"invalid_api_key",
			"invalid proxy API key",
		)
		return
	}
	path := r.URL.Path
	for _, prefix := range []string{"/v1", "/backend-api/codex"} {
		if strings.HasPrefix(path, prefix+"/") {
			path = strings.TrimPrefix(path, prefix)
			break
		}
	}
	switch path {
	case "/models":
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		p.models(w, r)
	case "/responses", "/responses/compact":
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		p.responses(w, r, path == "/responses/compact")
	default:
		writeError(
			w,
			http.StatusNotFound,
			"not_found",
			"only Responses, compaction, and model discovery are supported",
		)
	}
}

func (p *proxy) authorized(r *http.Request) bool {
	if p.cfg.apiKey == "" {
		return true
	}
	value := r.Header.Get("Authorization")
	parts := strings.SplitN(value, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return false
	}
	expected := sha256.Sum256([]byte(p.cfg.apiKey))
	provided := sha256.Sum256([]byte(parts[1]))
	return subtle.ConstantTimeCompare(expected[:], provided[:]) == 1
}

func (p *proxy) responses(w http.ResponseWriter, r *http.Request, compact bool) {
	if encoding := r.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		writeError(
			w,
			http.StatusUnsupportedMediaType,
			"unsupported_encoding",
			"send uncompressed JSON",
		)
		return
	}
	if contentType := r.Header.Get("Content-Type"); contentType != "" {
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil || mediaType != "application/json" {
			writeError(
				w,
				http.StatusUnsupportedMediaType,
				"unsupported_media_type",
				"send application/json",
			)
			return
		}
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(
				w,
				http.StatusRequestEntityTooLarge,
				"request_too_large",
				"request exceeds 16 MiB",
			)
			return
		}
		writeError(
			w,
			http.StatusBadRequest,
			"invalid_request",
			"cannot read request body",
		)
		return
	}
	prepared, err := prepareRequest(body, compact)
	if err != nil {
		writeError(
			w,
			http.StatusBadRequest,
			"invalid_request",
			err.Error(),
		)
		return
	}
	path := "/responses"
	if compact {
		path += "/compact"
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	resp, err := p.doUpstream(
		ctx,
		r,
		path,
		prepared,
	)
	if err != nil {
		p.upstreamError(w, r, err)
		return
	}
	defer resp.Body.Close()
	reader := newIdleReader(resp.Body, p.cfg.idleTimeout, cancel)
	defer reader.stop()
	failed := resp.StatusCode < 200 || resp.StatusCode >= 300
	if failed || compact {
		p.forwardJSON(
			w,
			r,
			resp,
			reader,
		)
		return
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	// Some Codex gateways omit Content-Type even when returning valid SSE.
	if mediaType != "" && mediaType != "text/event-stream" {
		p.upstreamError(w, r, errors.New("upstream did not return an event stream"))
		return
	}
	copyResponseHeaders(w.Header(), resp.Header)
	if prepared.stream {
		streamReader := newResponseStreamReader(reader, prepared.metadata)
		p.forwardStream(
			w,
			r,
			resp,
			streamReader,
		)
		return
	}
	result, err := collectResponse(reader)
	if err != nil {
		var eventError *streamError
		if errors.As(err, &eventError) {
			writeJSON(w, eventError.status, eventError.body)
			return
		}
		p.upstreamError(w, r, err)
		return
	}
	if len(prepared.metadata) > 0 {
		result, err = responseMetadata(result, prepared.metadata)
		if err != nil {
			p.upstreamError(w, r, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, result)
}

func (p *proxy) models(w http.ResponseWriter, r *http.Request) {
	version := r.URL.Query().Get("client_version")
	nativeCatalog := version != ""
	if version == "" {
		version = p.cfg.clientVersion
	}
	path := "/models?" + url.Values{"client_version": []string{version}}.Encode()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	resp, err := p.doUpstream(
		ctx,
		r,
		path,
		preparedRequest{},
	)
	if err != nil {
		p.upstreamError(w, r, err)
		return
	}
	defer resp.Body.Close()
	reader := newIdleReader(resp.Body, p.cfg.idleTimeout, cancel)
	defer reader.stop()
	failed := resp.StatusCode < 200 || resp.StatusCode >= 300
	if nativeCatalog || failed {
		p.forwardJSON(
			w,
			r,
			resp,
			reader,
		)
		return
	}
	body, err := readBounded(reader, maxJSONBytes)
	if err != nil {
		p.upstreamError(w, r, err)
		return
	}
	// Nil detects a missing/null required catalog field, not an API output array.
	catalog := struct {
		Models []struct {
			Slug string `json:"slug"`
		} `json:"models"`
	}{}
	if err := json.Unmarshal(body, &catalog); err != nil || catalog.Models == nil {
		p.upstreamError(w, r, errors.New("invalid upstream model catalog"))
		return
	}
	models := make([]map[string]any, 0, len(catalog.Models))
	for _, model := range catalog.Models {
		models = append(models, map[string]any{
			"id": model.Slug, "object": "model", "created": 0, "owned_by": "openai",
		})
	}
	result, _ := json.Marshal(map[string]any{"object": "list", "data": models})
	copyResponseHeaders(w.Header(), resp.Header)
	writeJSON(w, http.StatusOK, result)
}

func (p *proxy) doUpstream(
	ctx context.Context,
	incoming *http.Request,
	path string,
	prepared preparedRequest,
) (*http.Response, error) {
	creds, err := p.auth.current(ctx)
	if err != nil {
		return nil, err
	}
	refreshed := false
	for attempt := range 3 {
		req, err := http.NewRequestWithContext(
			ctx,
			incoming.Method,
			p.cfg.upstream+path,
			bytes.NewReader(prepared.body),
		)
		if err != nil {
			return nil, errors.New("cannot construct upstream request")
		}
		p.setUpstreamHeaders(
			req,
			incoming.Header,
			creds,
			prepared,
		)
		resp, err := p.client.Do(req)
		if err != nil {
			// Do not log URL-bearing transport errors (model queries can be arbitrary).
			return nil, errors.New("upstream transport failure")
		}
		lastAttempt := attempt == 2 || refreshed
		if resp.StatusCode != http.StatusUnauthorized || lastAttempt {
			return resp, nil
		}
		_ = resp.Body.Close()
		creds, refreshed, err = p.auth.recover(ctx, creds.accessToken)
		if err != nil {
			return nil, err
		}
	}
	return nil, errors.New("upstream authentication recovery exhausted")
}

func (p *proxy) setUpstreamHeaders(
	req *http.Request,
	incoming http.Header,
	creds credentials,
	prepared preparedRequest,
) {
	// Explicit allowlist: client API keys, cookies, account overrides and
	// hop-by-hop headers must not cross the credential boundary.
	for _, name := range []string{
		"Session-Id", "Session_id", "Conversation_id", "Thread-Id", "Version",
		"X-Client-Request-Id", "X-Codex-Turn-State", "X-Codex-Turn-Metadata",
		"X-Codex-Parent-Thread-Id", "X-Codex-Window-Id", "X-Codex-Beta-Features",
		"X-Openai-Internal-Codex-Responses-Lite", "X-Openai-Subagent", "X-Oai-Attestation",
	} {
		if connectionHeader(incoming, name) {
			continue
		}
		if value := incoming.Get(name); value != "" {
			req.Header.Set(name, value)
		}
	}
	req.Header.Set("Authorization", "Bearer "+creds.accessToken)
	req.Header.Set("Chatgpt-Account-Id", creds.accountID)
	req.Header.Set("User-Agent", p.cfg.userAgent)
	req.Header.Set("Originator", "codex_cli_rs")
	if req.Header.Get("Version") == "" {
		req.Header.Set("Version", p.cfg.clientVersion)
	}
	req.Header.Set("Accept", "application/json")
	if req.Method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	if strings.HasSuffix(req.URL.Path, "/responses") {
		req.Header.Set("Accept", "text/event-stream")
	}
	if prepared.model != "" {
		hint := "model=" + prepared.model
		if prepared.tier != "" {
			hint += ";tier=" + prepared.tier
		}
		req.Header.Set("X-Codex-Routing-Hint", hint)
	}
	if strings.HasSuffix(req.URL.Path, "/models") {
		if !connectionHeader(incoming, "If-None-Match") {
			if etag := incoming.Get("If-None-Match"); etag != "" {
				req.Header.Set("If-None-Match", etag)
			}
		}
	}
}

func (p *proxy) forwardJSON(w http.ResponseWriter, r *http.Request, resp *http.Response, reader io.Reader) {
	body, err := readBounded(reader, maxJSONBytes)
	if err != nil {
		p.upstreamError(w, r, err)
		return
	}
	copyResponseHeaders(w.Header(), resp.Header)
	w.Header().Set("Content-Type", "application/json")
	if contentType := resp.Header.Get("Content-Type"); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Second))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

func (p *proxy) forwardStream(w http.ResponseWriter, r *http.Request, resp *http.Response, reader io.Reader) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if err := controller.Flush(); err != nil {
		return
	}
	buffer := make([]byte, 32<<10)
	for {
		n, readErr := reader.Read(buffer)
		if n > 0 {
			_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if _, err := w.Write(buffer[:n]); err != nil {
				return
			}
			if err := controller.Flush(); err != nil {
				return
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) && r.Context().Err() == nil {
				p.logger.Warn("upstream stream interrupted")
				// Once 200 has been committed, abort rather than end a truncated
				// stream cleanly or inject synthetic protocol events.
				panic(http.ErrAbortHandler)
			}
			return
		}
	}
}

func (p *proxy) upstreamError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	p.logger.Warn("upstream request failed", "error", err)
	writeError(
		w,
		http.StatusBadGateway,
		"upstream_error",
		"Codex upstream request failed; check proxy logs",
	)
}

func copyResponseHeaders(destination, source http.Header) {
	for name, values := range source {
		if connectionHeader(source, name) {
			continue
		}
		lower := strings.ToLower(name)
		requestMetadata := false
		switch lower {
		case "x-request-id", "retry-after", "etag":
			requestMetadata = true
		}
		codexMetadata := strings.HasPrefix(lower, "x-codex-") || strings.HasPrefix(lower, "x-ratelimit-")
		if requestMetadata || codexMetadata {
			destination[name] = append([]string{}, values...)
		}
	}
}

func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	writeError(
		w,
		http.StatusMethodNotAllowed,
		"method_not_allowed",
		"use "+method,
	)
	return false
}

func writeJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Second))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	errorType := "invalid_request_error"
	switch {
	case status == http.StatusUnauthorized:
		errorType = "authentication_error"
	case status == http.StatusTooManyRequests:
		errorType = "rate_limit_error"
	case status >= 500:
		errorType = "server_error"
	}
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{"type": errorType, "code": code, "message": message, "param": nil},
	})
	writeJSON(w, status, body)
}

func connectionHeader(headers http.Header, name string) bool {
	for _, value := range headers.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), name) {
				return true
			}
		}
	}
	return false
}
