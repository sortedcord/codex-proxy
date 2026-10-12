# Codex Authentication and Routing Specification

**Document Version:** 8.x Architecture Reference  
**Scope:** OpenAI Codex (`codex`) Provider Subsystem (Distinct from ChatGPT Web & Standard OpenAI Platform)  
**Primary Source Trees:**  
- `internal/auth/codex/` (OAuth2 PKCE, Token Serialization, JWT Introspection)
- `sdk/auth/codex*.go` (Authenticator Implementations, Device Flow, Credential Records)
- `internal/runtime/executor/codex_*.go` (HTTP & WebSocket Executors, Session Stores, Duplex Streaming)
- `internal/runtime/executor/helps/` (Headers, Replay Caching, Multi-Agent V2, Tool Schemas)
- `internal/client/codex/` (Live WebRTC Relay, Model Catalogs, Tool Adapters)
- `internal/registry/` (Model Information, Codex Catalog Updater, Quota State)
- `sdk/cliproxy/auth/` (Conductor, Scheduler, Sharding, Selection Strategies, Session Affinity)
- `internal/translator/codex/` (Bidirectional Format Translation)

---

## 1. Architectural Overview & Codex Distinction

### 1.1 What is "Codex" in CLIProxyAPI?
In CLIProxyAPI, **`codex`** refers strictly to the API backend and protocol family engineered for **OpenAI Codex CLI (`codex-rs` / `codex-tui`)** and coding agents (e.g., GitHub Copilot CLI, OpenAI Responses API agents), rather than consumer ChatGPT web automation or standard OpenAI Developer Platform endpoints (`api.openai.com/v1`).

The Codex backend operates against OpenAI's backend infrastructure:
- **Primary Upstream Base URL:** `https://chatgpt.com/backend-api/codex`
- **Primary Responses Endpoint:** `/responses` (HTTP/SSE) and `/responses/compact` (JSON)
- **Primary Responses WebSocket:** `wss://chatgpt.com/backend-api/codex/responses`
- **Standalone Search Endpoint:** `https://chatgpt.com/backend-api/codex/alpha/search`
- **WebRTC / Realtime Live Endpoint:** `https://chatgpt.com/backend-api/codex/realtime/calls?intent=quicksilver&architecture=avas`

```
┌────────────────────────────────────────────────────────────────────────────────────────────────────────┐
│                                       DOWNSTREAM CLIENT INGRESS                                        │
│  - Codex CLI / TUI (`/backend-api/codex/*` or `/v1/responses`)                                        │
│  - Claude Code / Anthropic SDK (`/v1/messages`)                                                        │
│  - OpenAI SDK / Chat Completions (`/v1/chat/completions`)                                              │
│  - Gemini SDK (`/v1beta/models/*:generateContent`)                                                     │
│  - Realtime Audio / WebRTC (`/v1/realtime`, `/v1/live`)                                                │
└───────────────────────────────────────────────┬────────────────────────────────────────────────────────┘
                                                │
                                                ▼
┌────────────────────────────────────────────────────────────────────────────────────────────────────────┐
│                                     API LAYER & MODEL ROUTER                                           │
│  - `internal/api/server_routes.go`: Route registration & direct aliases                                │
│  - `sdk/api/handlers/handlers_routing.go`: Model Router plugin interception                            │
│  - `internal/util/provider.go`: `GetProviderName(model)` resolution via Model Registry                 │
└───────────────────────────────────────────────┬────────────────────────────────────────────────────────┘
                                                │
                                                ▼
┌────────────────────────────────────────────────────────────────────────────────────────────────────────┐
│                              AUTH CONDUCTOR & CREDENTIAL SCHEDULER                                     │
│  - Credentials Pool: OAuth Accounts (`auths/*.json`) + Config API Keys (`CodexKey`)                    │
│  - Strategy Selection: Round-Robin, Weighted Round-Robin, Fill-First                                   │
│  - Filter: `preferCodexWebsocketAuths`, `isCodexFreePlanAuth` exclusion                                │
│  - Session Affinity: `Session-Id`, `x-codex-parent-thread-id` subagent binding                          │
└───────────────────────────────────────────────┬────────────────────────────────────────────────────────┘
                                                │
                                                ▼
┌────────────────────────────────────────────────────────────────────────────────────────────────────────┐
│                             UPSTREAM EXECUTOR LAYER (CodexAutoExecutor)                                │
│                                                                                                        │
│       ┌────────────────────────────────────────────────┐   ┌─────────────────────────────────────────┐ │
│       │               CodexExecutor                    │   │         CodexWebsocketsExecutor         │ │
│       │             (HTTP / SSE Stream)                │   │         (Full-Duplex WebSocket)         │ │
│       ├────────────────────────────────────────────────┤   ├─────────────────────────────────────────┤ │
│       │ • Upstream: `/responses` or `/compact`         │   │ • Upstream: `wss://.../responses`       │ │
│       │ • Protocol Translators (Claude/OpenAI->Codex)   │   │ • `response.create` framing             │ │
│       │ • Stream Bootstrap Buffering (overload retry)  │   │ • Full-duplex `response.steer` dispatch │ │
│       │ • Reasoning Replay Cache (`cpa_codex_replay`)  │   │ • Out-of-band `response.interrupt`      │ │
│       │ • Cloaking & Identity Emulation Headers        │   │ • Websocket Session Store (`connMu`)    │ │
│       └────────────────────────────────────────────────┘   └─────────────────────────────────────────┘ │
└───────────────────────────────────────────────┬────────────────────────────────────────────────────────┘
                                                │
                                                ▼
┌────────────────────────────────────────────────────────────────────────────────────────────────────────┐
│                                    OPENAI CODEX UPSTREAM BACKEND                                       │
│                         `https://chatgpt.com/backend-api/codex`                                        │
└────────────────────────────────────────────────────────────────────────────────────────────────────────┘
```

---

## 2. Authentication Architecture

CLIProxyAPI supports three credential types for Codex:
1. **Codex OAuth2 Credentials** (Personal, Plus, Pro, Team accounts via OpenAI Auth)
2. **Codex API Key Credentials** (Custom enterprise or proxy base URLs via `CodexKey`)
3. **Realtime Ephemeral Client Secrets** (Short-lived `ek_` tokens for WebRTC/Realtime)

---

### 2.1 Credential Data Structures & File Storage

#### 2.1.1 OAuth Token Storage Structs (`internal/auth/codex/token.go` & `openai.go`)
```go
type CodexTokenStorage struct {
    IDToken      string         `json:"id_token"`
    AccessToken  string         `json:"access_token"`
    RefreshToken string         `json:"refresh_token"`
    AccountID    string         `json:"account_id"`
    LastRefresh  string         `json:"last_refresh"`
    Email        string         `json:"email"`
    Type         string         `json:"type"`          // Always "codex"
    Expire       string         `json:"expired"`       // RFC3339 timestamp
    PlanType     string         `json:"plan_type,omitempty"` // "free", "plus", "pro", "team"
    Metadata     map[string]any `json:"-"`
}
```

#### 2.1.2 Credential Filename Generation (`internal/auth/codex/filename.go`)
When persisted under the configured `auth-dir` (default: `auths/`), credential filenames follow deterministic naming:
```go
func CredentialFileName(email, planType, hashAccountID string, sanitize bool) string
```
- **Naming Pattern:** `codex-<email>-<plan_type>-<hashAccountID>.json`
- **Example:** `codex-developer@example.com-plus-3f9a1c2b.json`
- `hashAccountID` is the first 8 hex characters of the SHA-256 digest of the OpenAI `chatgpt_account_id`.

#### 2.1.3 Storage Flattening & Metadata Injection
When serialized via `SaveTokenToFile()`, extra runtime metadata (e.g. `websockets: true`, custom headers, proxy settings) is flattened directly into the root JSON object via `misc.MergeMetadata()`.

---

### 2.2 OAuth2 Authorization Code Flow with PKCE

The primary interactive login flow (`sdk/auth/codex.go`) emulates the native Codex CLI authentication.

```
User CLI / Admin             CLIProxyAPI (Local)                  OpenAI Auth Service
    │                                │                                     │
    │  1. Initiate Login             │                                     │
    ├───────────────────────────────>│                                     │
    │                                │  2. Generate PKCE (S256) & State    │
    │                                │  3. Start Local HTTP Server (:1455) │
    │                                │                                     │
    │  4. Open Browser / SSH Tunnel  │                                     │
    │<───────────────────────────────┤                                     │
    │                                │                                     │
    │  5. User Authenticates in Browser via Auth URL                       │
    ├─────────────────────────────────────────────────────────────────────>│
    │                                                                      │
    │  6. Redirect to http://localhost:1455/auth/callback?code=...&state=.. │
    ├───────────────────────────────>│                                     │
    │                                │                                     │
    │                                │  7. POST https://auth.openai.com/oauth/token
    │                                │     grant_type=authorization_code   │
    │                                │     code_verifier=<PKCEVerifier>    │
    │                                ├────────────────────────────────────>│
    │                                │                                     │
    │                                │  8. Token Response (JWT ID/Access/Refresh)
    │                                │<────────────────────────────────────┤
    │                                │                                     │
    │                                │  9. Parse Claims & Write auth file  │
    │  10. Auth Successful Record    │                                     │
    │<───────────────────────────────┤                                     │
```

#### 2.2.1 Protocol Constants (`internal/auth/codex/openai_auth.go`)
| Parameter | Value | Description |
| :--- | :--- | :--- |
| **Authorize Endpoint** | `https://auth.openai.com/oauth/authorize` | OpenAI authorization UI |
| **Token Endpoint** | `https://auth.openai.com/oauth/token` | OAuth2 token exchange |
| **Client ID** | `app_EMoamEEZ73f0CkXaXp7hrann` | Hardcoded official Codex CLI client ID |
| **Default Redirect URI** | `http://localhost:1455/auth/callback` | Local callback listener |
| **OAuth Scopes** | `openid email profile offline_access` | Required scopes for identity & refresh |
| **Refresh Timeout** | `30s` | Bounded via `context.WithoutCancel` |

#### 2.2.2 Authorization URL Parameters
The authorization URL constructed by `GenerateAuthURL()` includes:
```url
https://auth.openai.com/oauth/authorize?
  client_id=app_EMoamEEZ73f0CkXaXp7hrann&
  response_type=code&
  redirect_uri=http%3A%2F%2Flocalhost%3A1455%2Fauth%2Fcallback&
  scope=openid+email+profile+offline_access&
  state=<RandomState32>&
  code_challenge=<Base64URL(SHA256(CodeVerifier))>&
  code_challenge_method=S256&
  prompt=login&
  id_token_add_organizations=true&
  codex_cli_simplified_flow=true
```
- `id_token_add_organizations=true`: Instructs OpenAI Auth to embed multi-tenant organization claims in the ID token.
- `codex_cli_simplified_flow=true`: Triggers OpenAI's streamlined login UX designed for command-line tools.

#### 2.2.3 Local Callback Server & Fallback Prompt
1. **Server Lifecycle:** Spins up an HTTP server on port `1455` (configurable via `opts.CallbackPort`). If port `1455` is occupied, it returns `ErrPortInUse`.
2. **Headless & SSH Tunnel Mode:** If `browser.IsAvailable()` returns `false` or opening fails, CLIProxyAPI prints SSH port-forwarding instructions:
   ```bash
   ssh -L 1455:localhost:1455 <user>@<remote-host>
   ```
3. **Manual Paste Fallback:** After 15 seconds (`opts.Prompt != nil`), an async prompt is displayed in the terminal:
   `"Paste the Codex callback URL (or press Enter to keep waiting): "`
   Callers can paste the full callback URL copied from remote environments.

---

### 2.3 OAuth2 Device Code Flow

When operating in environments where a local callback port cannot be bound, CLIProxyAPI supports the Device Authorization flow (`sdk/auth/codex_device.go`).

#### 2.3.1 Activation Condition
Activated when `opts.Metadata["codex_login_mode"] == "device"`.

#### 2.3.2 Endpoints and Polling
1. **User Code Acquisition:**
   - `POST https://auth.openai.com/api/accounts/deviceauth/usercode`
   - Payload: `{"client_id": "app_EMoamEEZ73f0CkXaXp7hrann"}`
   - Response: Returns `device_auth_id`, `user_code`, `interval` (default 5 seconds).
2. **User Interaction:**
   - Verification URL: `https://auth.openai.com/codex/device`
   - User navigates to the URL and enters the displayed `user_code`.
3. **Polling Loop:**
   - `POST https://auth.openai.com/api/accounts/deviceauth/token`
   - Payload: `{"device_auth_id": "...", "user_code": "..."}`
   - Polls every `interval` seconds until status `200 OK` or deadline (`15 minutes`).
   - Response provides: `authorization_code`, `code_verifier`, `code_challenge`.
4. **Token Exchange:**
   - Swaps the authorization code using `ExchangeCodeForTokensWithRedirect()` with redirect URI:  
     `https://auth.openai.com/deviceauth/callback`

---

### 2.4 JWT Token Introspection & Claims Parsing

Codex ID tokens are JSON Web Tokens (JWT) issued by OpenAI. CLIProxyAPI parses the payload without cryptographic verification (`internal/auth/codex/jwt_parser.go`) to extract tenant and subscription claims:

```go
type JWTClaims struct {
    Email         string        `json:"email"`
    CodexAuthInfo CodexAuthInfo `json:"https://api.openai.com/auth"`
}

type CodexAuthInfo struct {
    ChatgptAccountID string          `json:"chatgpt_account_id"`
    ChatgptPlanType  string          `json:"chatgpt_plan_type"`  // "free", "plus", "pro", "team"
    ChatgptUserID    string          `json:"chatgpt_user_id"`
    Organizations    []Organizations `json:"organizations"`
}
```

#### Extraction Rules:
- **Account ID:** Extracted from `claims.CodexAuthInfo.ChatgptAccountID`. Used later to inject the upstream `Chatgpt-Account-Id` header.
- **Plan Type:** Extracted from `claims.CodexAuthInfo.ChatgptPlanType`. If empty or missing, defaults to `DefaultPlanType = "free"`.
- **Email:** Extracted from `claims.Email`.

---

### 2.5 Token Refresh Pipeline & Concurrency Control

#### 2.5.1 Singleflight Deduplication & Retries (`internal/auth/codex/openai_auth.go`)
- **Singleflight Group:** All concurrent token refresh requests sharing the same refresh token are coalesced via `var codexRefreshGroup singleflight.Group`.
- **Isolated Context:** Executes within `context.WithoutCancel(ctx)` with a strict `30s` timeout (`codexRefreshTimeout`) to ensure downstream HTTP request cancellations do not terminate an in-flight upstream token refresh.
- **Retry Logic:** `RefreshTokensWithRetry()` provides up to 3 attempts with exponential backoff (`time.Duration(attempt) * time.Second`).
- **Fatal Error Detection:** Non-retryable errors containing `"refresh_token_reused"` immediately abort without retrying to prevent credential corruption.

#### 2.5.2 Executor Refresh Hook (`internal/runtime/executor/codex_executor_auth.go`)
1. Checks for control-plane centralized refresh via `helps.RefreshAuthViaHome(ctx, e.cfg, auth)`.
2. Reads `auth.Metadata["refresh_token"]`.
3. Issues refresh using the proxy configured for the specific credential (`auth.ProxyURL`).
4. Updates `auth.Metadata`: `access_token`, `id_token`, `refresh_token`, `account_id`, `expired`, `last_refresh`, and `plan_type`.
5. Updates in-memory `auth.Storage.(*CodexTokenStorage)` and sets `auth.Attributes["plan_type"]`.

---

### 2.6 Realtime Ephemeral Client Secret Authentication

For WebRTC and Realtime calls (`/v1/realtime/*`, `/v1/live/*`), CLIProxyAPI provides a local token store (`internal/client/codex/live/client_secret.go`):
- **Token Format:** `ek_<32-byte-base64url-payload>` (e.g., `ek_...`).
- **Session Identification:** Associated with a virtual session `sess_<18-byte-base64url-payload>`.
- **Lifetime Bounds:** Default: 10 minutes; Min: 10 seconds; Max: 2 hours.
- **Capacity Bounds:** Max 1024 total active entries; Max 64 active entries per issuer principal.
- **Authentication:** Validated via incoming `Authorization: Bearer ek_...`. Populates Gin context keys:
  - `ClientSecretSessionContextKey` (`codexLiveClientSecretSession`)
  - `ClientSecretPrincipalContextKey` (`codexLiveClientSecretPrincipal`)

---

## 3. Identity Emulation & Cloaking

Upstream ChatGPT backend servers employ Cloudflare and strict header validation rules. To prevent blocks and ensure proper priority routing, CLIProxyAPI implements an identity emulation layer.

### 3.1 Emulated Headers Matrix

| Header Name | Injected Value | When Injected | Condition / Overrides |
| :--- | :--- | :--- | :--- |
| **`User-Agent`** | `codex-tui/0.154.0 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.154.0)` | All OAuth HTTP & WebSocket requests | Overridden by `cfg.CodexHeaderDefaults.UserAgent` or disabled by cloaking opt-out |
| **`Originator`** | `codex-tui` | All OAuth requests | Suppressed for API-key credentials |
| **`Chatgpt-Account-Id`** | Value from `auth.Metadata["account_id"]` | All OAuth requests | Suppressed for API-key credentials |
| **`X-Codex-Routing-Hint`** | `model=<slug>;tier=<service_tier>` | All OAuth requests | Derived from upstream body model and tier; suppressed for API keys |
| **`OpenAI-Beta`** | `responses_websockets=2026-02-06` | WebSocket handshakes | Ensures upstream WebSocket feature negotiation |
| **`Session-Id` / `session_id`** | Extracted session UUID | HTTP and WebSocket | Case-preserved; converted to `session_id` on WebSockets |
| **`Conversation_id`** | Same as `Session-Id` | WebSocket handshakes | Mirrors session ID for multi-turn tracking |
| **`X-OpenAI-Internal-Codex-Responses-Lite`** | Downstream header value | Native Codex requests | Propagated when present in downstream request |

### 3.2 Dynamic Routing Hint Generation (`internal/runtime/executor/codex_executor_request.go`)
```go
func applyCodexRoutingHint(ctx context.Context, headers http.Header, auth *cliproxyauth.Auth, baseModel string, upstreamBody []byte, clientHeaders http.Header)
```
- Emulates Rust Codex (`codex-rs/core/src/client.rs`).
- Automatically extracts the resolved model and `service_tier` from the final mutated payload:
  - Base: `model=gpt-5.5`
  - With tier: `model=gpt-5.5;tier=priority` or `model=gpt-5.5;tier=ultrafast`
- If the operator configured custom header rules (`header:X-Codex-Routing-Hint`), the operator's value takes precedence.

### 3.3 Cloaking Opt-Out (`isCodexCloakingDisabled`)
Cloaking can be disabled at three granularities:
1. **Per-Credential Attribute:** `auth.Attributes["codex_disable_cloaking"] = "true"`
2. **Per-API-Key Config:** `CodexKey.DisableCodexCloaking: true`
3. **Global Config:** `cfg.Codex.DisableCodexCloaking: true`

When cloaking is disabled, CLIProxyAPI refrains from forcing `codex-tui` User-Agent/Originator and forwards the downstream client's raw headers.

---

## 4. Routing Architecture & Upstream Endpoints

### 4.1 Route Surface Map

```
Incoming Request
 │
 ├── GET/POST `/backend-api/codex/responses` ───────► openaiResponsesHandlers.ResponsesWebsocket / Responses
 ├── POST     `/backend-api/codex/responses/compact` ─► openaiResponsesHandlers.Compact
 ├── POST     `/backend-api/codex/alpha/search` ─────► s.codexAlphaSearch
 │
 ├── GET/POST `/v1/responses` ──────────────────────► openaiResponsesHandlers.ResponsesWebsocket / Responses
 ├── POST     `/v1/responses/compact` ──────────────► openaiResponsesHandlers.Compact
 ├── POST     `/v1/alpha/search` ───────────────────► s.codexAlphaSearch
 │
 ├── POST     `/v1/chat/completions` ───────────────► Translate to Codex if model provider is "codex"
 ├── POST     `/v1/messages` ───────────────────────► Translate to Codex if model provider is "codex"
 ├── POST     `/v1beta/models/*` ───────────────────► Translate to Codex if model provider is "codex"
 │
 ├── GET      `/v1/models?client_version=...` ──────► Serve specialized Codex Client Catalog
 │
 └── WebRTC   `/v1/realtime/*`, `/v1/live/*` ───────► s.codexLiveHandler (Pion Media Relay + Sideband)
```

---

### 4.2 Upstream Transport Selection (`CodexAutoExecutor`)

The `CodexAutoExecutor` (`internal/runtime/executor/codex_websockets_executor.go`) acts as the fronting executor for provider `"codex"`. It dynamically chooses between HTTP/SSE and WebSockets:

```go
func (e *CodexAutoExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
    if cliproxyexecutor.DownstreamWebsocket(ctx) && codexWebsocketsEnabled(auth) {
        return e.wsExec.Execute(ctx, auth, req, opts)
    }
    if cliproxyexecutor.RequiredUpstreamWebsocket(ctx) {
        return cliproxyexecutor.Response{}, cliproxyexecutor.NewUpstreamWebsocketReplayRequiredError()
    }
    return e.httpExec.Execute(ctx, auth, req, opts)
}
```

#### Decision Rules:
1. **Downstream is WebSocket & Credential Supports WebSocket:**
   - Routes to `CodexWebsocketsExecutor` (`wss://chatgpt.com/backend-api/codex/responses`).
2. **Downstream is WebSocket & Credential Does NOT Support WebSocket:**
   - If downstream strictly requires upstream WebSocket (`RequiredUpstreamWebsocket`), returns `UpstreamWebsocketReplayRequiredError`, signaling the Conductor to failover to a WebSocket-capable credential.
3. **Downstream is HTTP/SSE:**
   - Routes to `CodexExecutor` via HTTPS POST to `/responses` or `/responses/compact`.

---

### 4.3 WebSocket Duplex Execution (`streamCodexDuplex`)

When operating in full-duplex WebSocket mode (`internal/runtime/executor/codex_websockets_duplex.go`), the executor manages continuous two-way communication:

```
Downstream Client                  CLIProxyAPI                     OpenAI Upstream WebSocket
        │                               │                                     │
        │ 1. Connect WebSocket          │                                     │
        ├──────────────────────────────>│ 2. Dial Upstream WebSocket          │
        │                               ├────────────────────────────────────>│
        │ 3. WebSocket Handshake OK     │                                     │
        │<──────────────────────────────┤                                     │
        │                               │                                     │
        │ 4. `response.create` (Frame)  │ 5. Frame & Forward Request          │
        ├──────────────────────────────>├────────────────────────────────────>│
        │                               │                                     │
        │                               │ 6. `response.created`               │
        │ 7. Stream Events              │<────────────────────────────────────┤
        │<──────────────────────────────┤                                     │
        │                               │ 8. `response.output_item.added`...  │
        │                               │<────────────────────────────────────┤
        │                               │                                     │
        │ 8. `response.steer` (Turn)    │ 9. Ingest Steer & Forward           │
        ├──────────────────────────────>├────────────────────────────────────>│
        │                               │                                     │
        │ 10. `response.interrupt` (OOB)│ 11. Forward Control Frame           │
        ├──────────────────────────────>├────────────────────────────────────>│
        │                               │                                     │
        │                               │ 12. `response.completed`            │
        │ 13. Completion & Usage Sync   │<────────────────────────────────────┤
        │<──────────────────────────────┤                                     │
```

#### Key Capabilities of Duplex Session:
- **`response.steer` Handling:** Client can submit mid-turn steer requests without tearing down the socket.
- **`response.interrupt` Handling:** Client can interrupt an in-progress generation (`InterruptExecutionSession()`).
- **Connection Isolation (`codexDuplexConnectionError`):** Network errors occurring *after* the initial turn handshake are treated as request-scoped, preventing the Conductor from inappropriately placing the shared account into cooldown.

---

### 4.4 Standalone Alpha Search Route (`s.codexAlphaSearch`)

Codex clients utilize an independent search endpoint (`/backend-api/codex/alpha/search` or `/v1/alpha/search`) for documentation and code search:
- **No Translation:** The payload is already in native Codex search format; bypasses protocol translators.
- **Sanitization:** Strips `prompt_cache_key` and `prompt_cache_retention` before forwarding.
- **Model Rewrite:** Rewrites the payload's `model` parameter to the upstream model resolved by the auth manager.
- **Upstream Target:**
  - OAuth: `https://chatgpt.com/backend-api/codex/alpha/search`
  - API Key: `<base-url>/alpha/search` (requires `alpha-search: true` on `CodexKey`).

---

### 4.5 WebRTC Realtime / Live Gateway (`internal/client/codex/live`)

Codex Live provides interactive voice and multimodal capabilities:
1. **WebRTC Call Bootstrap:** Downstream clients issue an HTTP POST with SDP offer to `/v1/realtime/calls` or `/v1/live`.
2. **Upstream Request:** Forwarded to `https://chatgpt.com/backend-api/codex/realtime/calls?intent=quicksilver&architecture=avas`.
3. **Pion Media Relay:** If `live-media-relay.enabled: true`, CLIProxyAPI uses an in-process Pion WebRTC stack to terminate downstream media, rewrite SDP candidates to its public IP, allocate UDP ports (`udp-port-min` to `udp-port-max`), and relay RTP streams.
4. **Sideband WebSocket:** Provides event and data channels on `/v1/realtime/calls/:call_id` and `/v1/live/:call_id`.
5. **Call Hangup:** Forwarded to `/realtime/calls/<call_id>/hangup` pinned to the original session's OAuth credential.

---

## 5. Conductor Scheduling, Load Balancing & Failover

The central Conductor (`sdk/cliproxy/auth/`) governs credential selection, rotation, session affinity, and fault tolerance for Codex.

---

### 5.1 Credential Selection Strategies

CLIProxyAPI supports three scheduling algorithms configured via `routing.strategy`:

1. **Round-Robin (`RoundRobinSelector`):**
   - Rotates by credential ID (`successorIndex`) across the active pool rather than relying on monotonic index offsets.
   - Preserves fair rotation even as failed credentials enter cooldown.
2. **Smooth Weighted Round-Robin (`WeightedRoundRobinSelector`):**
   - Applies NGINX-style smooth weighted deficit round-robin based on `weight` configured on each credential.
3. **Fill-First (`FillFirstSelector`):**
   - Deterministically routes to the first available credential until its quota/rate-limit is exhausted before rotating.
   - Maximizes rolling-window utilization for individual subscription tiers.

---

### 5.2 Codex-Specific Selector Filters

In all selector strategies, `available` credentials pass through two Codex-specific filters:

#### 1. WebSocket Transport Preference (`preferCodexWebsocketAuths`)
```go
func preferCodexWebsocketAuths(ctx context.Context, provider string, available []*Auth) []*Auth
```
If downstream is an active WebSocket (`cliproxyexecutor.DownstreamWebsocket(ctx) == true`), the candidate pool is filtered to keep only credentials with `authWebsocketsEnabled(candidate) == true`. If none exist, it falls back to the full pool.

#### 2. Free Plan Disallowance (`isCodexFreePlanAuth`)
If the execution context or model requires advanced tools/reasoning unavailable on free tiers, credentials with `plan_type == "free"` are excluded from candidate selection.

---

### 5.3 Session Affinity & Subagent Inheritance

#### 5.3.1 Session ID Extraction Priority (`ExtractSessionID` in `selector.go`)
1. `X-Claude-Code-Session-Id`
2. Claude Code metadata session
3. **`Session-Id` / `Session_id` (Codex CLI native header)**
4. `X-Session-ID`
5. `X-Session-Affinity` (OpenCode)
6. `X-Client-Request-Id` (pi Responses)
7. `prompt_cache_key`
8. Derived context session identity

#### 5.3.2 Subagent Hierarchy Inheritance
Codex agents frequently spawn subagents. CLIProxyAPI inspects:
- `x-codex-parent-thread-id`
- `X-Codex-Parent-Thread-Id`
- `X-Parent-Thread-Id`

When a child request carries a parent thread ID, the selector locates the parent session's pinned credential and routes the subagent to the **exact same credential**, preserving context affinity and avoiding split-state quota penalties across accounts.

---

### 5.4 Failover & Fault Tolerance

```
[Request Start]
      │
      ▼
[Pick Credential A] ──► [Execute Stream with Bootstrap Buffering]
                             │
                             ├─► [Upstream 200 OK + Overload Event within Buffer]
                             │        │
                             │        ▼
                             │   [Release Buffer & Reset Headers]
                             │        │
                             │        ▼
                             │   [Cool Credential A for Model]
                             │        │
                             │        ▼
                             └───► [Failover: Pick Credential B] ──► [Forward to Client]
```

#### 5.4.1 Stream Bootstrap Buffering (`StreamBootstrapBuffering`)
- **Problem:** OpenAI Codex returns `HTTP 200 OK` and emits initial handshake frames (`response.created`, `response.in_progress`, keepalives), followed immediately by an in-band overload event (`{"type": "error", "error": {"code": "server_is_overloaded"}}`) instead of returning an HTTP 503 wire error. Normally, committing HTTP 200 headers prevents proxy retries.
- **Solution:** CLIProxyAPI holds back downstream response headers and buffers initial events up to:
  - **48 frames**, or
  - **1 MB of data**, or
  - Duration defined by `stream-bootstrap-timeout` (e.g. `10s`).
- If an overload or rate-limit error is received during the buffer window, the proxy discards the buffer, marks Credential A as overloaded, picks Credential B, and initiates a clean upstream request without the downstream client ever seeing an error.

#### 5.4.2 Model-Level Cooling (`ModelLevelCooling`)
- When `model-level-cooling: true`:
  - Upstream errors matching `usage_limit_reached` or HTTP 429 cool down **only the specific model** (e.g. `gpt-5.5`) on that credential.
  - Sibling models (e.g. `gpt-4o`, `gpt-5-mini`) on the same account remain active and eligible for routing.

#### 5.4.3 Reasoning Replay Cache (`internal/cache/codex_reasoning_replay_cache.go`)
- Multi-turn Codex interactions require replaying encrypted reasoning tokens (`reasoning.encrypted_content`).
- CLIProxyAPI maintains an in-memory / Home KV replay cache:
  - Key: `codex-reasoning-replay\x00<model>\x00<sessionKey>`
  - TTL: 1 hour; Max entries: 10,240; Max turns per entry: 256; Max bytes: 16 MB.
- **Signature Recovery:** If upstream returns a 4xx error indicating an invalid signature, CLIProxyAPI invokes `clearCodexReasoningReplayOnInvalidSignature()`, purging the cache entry so subsequent turns start with clean state.

---

## 6. Protocol & Payload Translation Mechanics

When non-Codex clients call CLIProxyAPI targeting Codex models, or when Codex CLI targets other backends, the translator subsystem performs format conversion.

### 6.1 Downstream Request Normalization for Codex Upstream

When converting requests into Codex Responses format (`ConvertOpenAIResponsesRequestToCodex`):

1. **Forced Flags:**
   - `stream: true` (unless routed to `/responses/compact`)
   - `store: false` (OpenAI store is unsupported by Codex backend)
   - `parallel_tool_calls: true`
2. **Stripped Parameters:**
   - Strips `max_output_tokens`, `max_completion_tokens`, `temperature`, `top_p`.
   - Strips `truncation`, `prompt_cache_options`, `prompt_cache_retention`.
   - Strips `user` field.
3. **Prompt Cache Breakpoint Stripping:**
   - Strips `"prompt_cache_breakpoint"` from input items (injected by GitHub Copilot CLI, which causes strict Codex upstreams to fail).
4. **Role Conversion:**
   - Converts `role: "system"` to `role: "developer"` in the `input` array.
5. **Empty Tool Call Arguments Repair:**
   - Rewrites parameter-less tool calls where `arguments: ""` to `arguments: "{}"` to satisfy strict JSON schema parsers.
6. **Include Directive:**
   - Sets `include: ["reasoning.encrypted_content"]` (and optionally `web_search_call.action.sources`).
7. **Service Tier Normalization:**
   - Maps `fast` or `priority` to `priority`.
   - Maps `ultrafast` to `ultrafast`.
   - Strips any unrecognized tiers.

---

### 6.2 Thinking & Reasoning Configuration (`internal/thinking/provider/codex/apply.go`)

Codex uses a nested `reasoning` structure:
```json
{
  "reasoning": {
    "effort": "high"
  }
}
```
- Discrete levels: `none`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max`, `ultra`.
- Numeric token budgets (e.g. Anthropic `budget_tokens`) are mapped to discrete effort levels via threshold mapping (`ConvertBudgetToLevel`).

---

### 6.3 Tool Adapters

#### 6.3.1 Apply-Patch Adapter (`internal/client/codex/apply-patch/tool.go`)
- Converts Codex's native freeform `apply_patch` grammar (`*** Begin Patch ... *** End Patch`) into a typed JSON tool declaration when bridging to clients that require strict JSON schemas.

#### 6.3.2 Multi-Agent V2 Adapter (`internal/client/codex/optimize-multi-agent-v2/`)
- Strips `parameters.properties.message.encrypted` from collaboration tools (`spawn_agent`, `send_message`, `followup_task`) so message text remains transparent to the proxy.
- Dynamically injects available models into `spawn_agent` descriptions.

#### 6.3.3 Image Generation Tool Injection
- If image generation is enabled, injects `{"type":"image_generation","output_format":"png"}` for supported models.

---

## 7. Model Catalog Generation for Codex Clients

When official Codex CLI clients request `/v1/models?client_version=<version>`:
1. `OpenAIModels()` detects `client_version` in the query string.
2. Invokes `BuildResponseForClient()` (`internal/client/codex/models/models.go`).
3. Loads template models from `internal/registry/models/codex_client_models.json` (auto-updated every 3 hours from remote).
4. **Client Version Filtering:**
   - If client version `< 0.144.0`, strips `max` and `ultra` reasoning levels (legacy client compatibility).
5. **Tool Search & Web Search Advertisement:**
   - Computes whether upstream credentials support search tools before advertising `supports_search_tool: true`.
6. **Compact Serialization:** Serializes using `MarshalCompact()` without HTML escaping to keep response bodies within Codex CLI's 1 MiB catalog limit.

---

## 8. Configuration Reference

Complete YAML specification for Codex-related configuration in `config.yaml`:

```yaml
# Global Upstream Configuration for Codex
upstream:
  codex:
    # Disable forcing official Codex identity headers (User-Agent, Originator, etc.)
    disable-codex-cloaking: false

    # Buffer early stream events to allow failover on in-band server_is_overloaded errors
    stream-bootstrap-buffering: true

    # Maximum duration to buffer bootstrap events before committing headers (e.g. "10s", "0" for unlimited)
    stream-bootstrap-timeout: "15s"

    # Scope quota cooldowns strictly to the requested model rather than cooling the entire account
    model-level-cooling: true

    # Enable full-duplex Codex WebSockets (response.steer & response.interrupt)
    response-steering: true

    # Opt-in compatibility for orphan Codex delegation outputs
    orphan-delegation-compatibility: false

    # Built-in WebRTC media relay configuration
    live-media-relay:
      enabled: false
      max-sessions: 32
      disable-private-remote-ips: false
      public-ip: "auto"
      udp-port-min: 20000
      udp-port-max: 20500
      ice-servers:
        - urls:
            - "stun:stun.l.google.com:19302"

# Client Compatibility Flags
client:
  codex:
    # Optimize multi-agent requests across providers
    optimize-multi-agent-v2: true

    # Advertise freeform apply_patch tool capability
    enable-apply-patch: true

# Header Defaults for Codex Upstream Requests
oauth:
  providers:
    codex:
      header-defaults:
        user-agent: "codex-tui/0.154.0 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.154.0)"
        beta-features: ""

# Static Codex API Keys (Optional Alternative to OAuth)
codex-api-key:
  - api-key: "sk-custom-codex-key"
    base-url: "https://chatgpt.com/backend-api/codex"
    priority: 10
    weight: 1
    prefix: "enterprise"
    websockets: true
    alpha-search: true
    disable-cooling: false
    disable-codex-cloaking: false
    request-retry: 2
    proxy-url: "socks5://127.0.0.1:1080"
    models:
      - name: "gpt-5.5"
        alias: "codex-main"
        is-compat: false
```

---

## 9. Verification & Architectural Constraints

1. **Internal Translator Isolation:** `internal/translator/` contains strictly pure transformation logic without network, credential, or storage dependencies.
2. **Payload Rule Barrier:** User-configured payload rules are applied as the final semantic transformation immediately before transport serialization in `internal/runtime/executor/`.
3. **Post-Connection Zero Timeouts:** Per architectural invariants, once an upstream connection or WebSocket handshake is established, no artificial wall-clock read/write timeouts are imposed except for WebSocket liveness ping/pong deadlines.
4. **Credential Isolation:** Refresh tokens, access tokens, and account IDs are never logged in plaintext; diagnostics utilize masked account summaries (`auth.AccountInfo()`).
