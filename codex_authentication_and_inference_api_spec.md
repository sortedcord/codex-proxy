# Codex Authentication and Inference API Specification

This specification provides a comprehensive, production-grade architectural and technical reference for how Codex (`codex-rs`) handles **Authentication** and **API Inference**. It is based on a direct exploration and analysis of the Codex codebase.

---

## Table of Contents

- [Codex Authentication and Inference API Specification](#codex-authentication-and-inference-api-spec)
  - [Table of Contents](#table-of-contents)
  - [Executive Architectural Overview](#1-executive-architectural-overview)
  - [Part I: Codex Authentication Architecture](#part-i-codex-authentication-architecture)
    - [1. Authentication Modes and Data Structures](#11-authentication-modes-and-data-structures)
    - [2. Credential Storage and Persistence (`auth.json` and Keyring)](#12-credential-storage-and-persistence-authjson-and-keyring)
    - [3. Interactive OAuth 2.0 PKCE Login Flow](#13-interactive-oauth-20-pkce-login-flow)
    - [4. Token Exchange Flow (RFC 8693)](#14-token-exchange-flow-rfc-8693)
    - [5. Token Refresh Architecture (Proactive and Reactive)](#15-token-refresh-architecture-proactive-and-reactive)
    - [6. External Host Authentication](#16-external-host-authentication)
    - [7. Agent Identity Architecture](#17-agent-identity-architecture)
    - [8. Workload Identity (Cloud and Machine Auth)](#18-workload-identity-cloud-and-machine-auth)
    - [9. Enterprise Gateway OAuth](#19-enterprise-gateway-oauth)
    - [10. Amazon Bedrock Authentication Matrix](#110-amazon-bedrock-authentication-matrix)
  - [Part II: Codex API for Inference Architecture](#part-ii-codex-api-for-inference-architecture)
    - [1. Model Provider Abstraction and Wire Protocol](#21-model-provider-abstraction-and-wire-protocol)
    - [2. Workspace Routing and Managed Residency](#22-workspace-routing-and-managed-residency)
    - [3. Model Discovery and Catalog Management (`/models`)](#23-model-discovery-and-catalog-management-models)
    - [4. Turn Pipeline and Prompt Construction](#24-turn-pipeline-and-prompt-construction)
    - [5. Request Header Assembly](#25-request-header-assembly)
    - [6. The Responses API Request Schema](#26-the-responses-api-request-schema)
    - [7. Dual-Transport Architecture: WebSocket vs HTTP/SSE](#27-dual-transport-architecture-websocket-vs-httpsse)
    - [8. Incremental WebSocket Turn Streaming](#28-incremental-websocket-turn-streaming)
    - [9. Streaming Event Stream Processing and Parsing](#29-streaming-event-stream-processing-and-parsing)
    - [10. Tool Execution, Turn Loop, and History Maintenance](#210-tool-execution-turn-loop-and-history-maintenance)
    - [11. Context Compaction and Context Window Management](#211-context-compaction-and-context-window-management)
    - [12. Error Recovery and Resilience State Machine](#212-error-recovery-and-resilience-state-machine)
  - [Part III: Complete End-to-End Sequence Diagram](#part-iii-complete-end-to-end-sequence-diagram)

---

## 1. Executive Architectural Overview

Codex is structured as a layered agent system implemented in Rust (`codex-rs`), orchestrated around two fundamental systems:
1. **The Authentication Subsystem (`codex-login`, `codex-aws-auth`, `codex-keyring-store`)**: Responsible for identity resolution, credential storage, interactive PKCE browser logins, token exchange, proactive/reactive token refreshing, multi-tenant workspace routing, agent cryptographic identity, cloud workload federation, and enterprise gateway OAuth.
2. **The Inference Subsystem (`codex-api`, `codex-model-provider`, `codex-client`, `codex-core`)**: Responsible for dispatching LLM generation requests exclusively via OpenAI's **Responses API** (`/v1/responses`), orchestrating dual-transport communication (Responses-over-WebSocket with HTTP/SSE fallback), performing incremental turn deltas over persistent WebSockets, parsing streaming Server-Sent Events, executing tools, managing context limits through compaction, and handling recoverable auth and rate-limit errors.

```
+----------------------------------------------------------------------------------------------------+
|                                             USER / UI                                              |
|                           (CLI / TUI, App-Server Daemon, IDE Extensions)                           |
+----------------------------------------------------------------------------------------------------+
                                                   |
                                                   v
+----------------------------------------------------------------------------------------------------+
|                                         CORE ORCHESTRATION                                         |
|                                    (Session, Turn, ThreadManager)                                  |
+----------------------------------------------------------------------------------------------------+
                 |                                                                    |
                 v                                                                    v
+----------------------------------+                                +--------------------------------+
|     AUTH SUBSYSTEM (codex-login) |                                |   INFERENCE SUBSYSTEM          |
|                                  |                                |   (codex-model-provider,       |
| - AuthManager (Arc, Shared Lock) |                                |    codex-api, codex-client)    |
| - Storage: ~/.codex/auth.json /  |                                |                                |
|   Keyring / In-Memory            |                                | - ModelProvider Trait          |
| - Modes: ApiKey, ChatGPT OAuth,  |                                | - ConfiguredModelProvider      |
|   AgentIdentity, WorkloadId,     |                                | - AmazonBedrockModelProvider   |
|   Headers, Bedrock               |                                | - Workspace Routing (Origin    |
| - Proactive Exp Refresh (<5 min) |                                |   Override, Account Pinning)   |
| - Reactive 401 Recovery          |                                | - Managed Residency (US)       |
| - OAuth PKCE Server (1455/1457)  |                                | - ModelClientSession           |
| - Token Exchange (RFC 8693)      |                                | - Dual Transport:              |
| - Gateway OAuth (Proxy/Cookie)   |                                |   * WebSocket (Incremental)   |
+----------------------------------+                                |   * HTTP/SSE (Fallback)        |
                 |                                                  +--------------------------------+
                 | Attaches Credentials / Headers                                     |
                 +--------------------------------------------------------------------+
                                                   |
                                                   v
+----------------------------------------------------------------------------------------------------+
|                                         REMOTE BACKENDS                                            |
|                                                                                                    |
| [OpenAI Responses API]                [ChatGPT Backend API]               [Amazon Bedrock Mantle/  |
| (https://api.openai.com/v1)           (https://chatgpt.com/backend-api/    Runtime OpenAi Endpoint]|
|                                        codex)                                                      |
+----------------------------------------------------------------------------------------------------+
```

---

# Part I: Codex Authentication Architecture

## 1.1 Authentication Modes and Data Structures

Codex formalizes user and machine identities through high-level protocol descriptors and runtime enum definitions.

### Protocol Representation: `AuthMode`
Located in `codex-rs/protocol/src/auth.rs`:
```rust
#[derive(Debug, Clone, Copy, PartialEq, Eq, Display, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum AuthMode {
    /// OpenAI API key provided by the caller and stored by Codex.
    ApiKey,
    /// ChatGPT OAuth managed by Codex (tokens persisted and refreshed by Codex).
    Chatgpt,
    /// ChatGPT auth tokens supplied by an external host application (e.g. IDE).
    #[serde(rename = "chatgptAuthTokens")]
    ChatgptAuthTokens,
    /// Codex backend auth supplied as arbitrary request headers.
    #[serde(rename = "headers")]
    Headers,
    /// Programmatic Codex auth backed by a registered Agent Identity.
    #[serde(rename = "agentIdentity")]
    AgentIdentity,
    /// Programmatic Codex auth backed by a personal access token.
    #[serde(rename = "personalAccessToken")]
    PersonalAccessToken,
    /// Amazon Bedrock bearer token managed by Codex.
    #[serde(rename = "bedrockApiKey")]
    BedrockApiKey,
    /// Amazon Bedrock AWS access keys managed by Codex.
    #[serde(rename = "bedrockAccessKeys")]
    BedrockAccessKeys,
}
```

Key classification semantics:
- `has_chatgpt_account()`: Returns `true` for `Chatgpt`, `ChatgptAuthTokens`, and `PersonalAccessToken`. These identities are bound to ChatGPT account/subscription tiers (Free, Plus, Pro, Business, Enterprise, Edu).
- `uses_codex_backend()`: Returns `true` for `Chatgpt`, `ChatgptAuthTokens`, `Headers`, `AgentIdentity`, and `PersonalAccessToken`. These modes route to the Codex internal backend (`https://chatgpt.com/backend-api/codex`) rather than direct standard developer API endpoints.

### Internal Runtime Representation: `CodexAuth`
Located in `codex-rs/login/src/auth/manager.rs`:
```rust
#[derive(Debug, Clone)]
pub enum CodexAuth {
    ApiKey(ApiKeyAuth),
    Chatgpt(ChatgptAuth),
    ChatgptAuthTokens(ChatgptAuthTokens),
    Headers(AuthHeaders),
    AgentIdentity(AgentIdentityAuth),
    PersonalAccessToken(PersonalAccessTokenAuth),
    BedrockApiKey(BedrockApiKeyAuth),
    BedrockAccessKeys(BedrockAccessKeysAuth),
}
```

Each variant encapsulates its dedicated state:
- `ApiKeyAuth`: Raw API key string (`sk-...`).
- `ChatgptAuth`: Wraps `ChatgptAuthState` (containing `Arc<Mutex<Option<AuthDotJson>>>`) and an account-bound HTTP client.
- `ChatgptAuthTokens`: Externally supplied OAuth tokens without local file ownership.
- `AuthHeaders`: Raw `HeaderMap` injected on every request.
- `AgentIdentityAuth`: Cryptographic key pair (PKCS#8 RSA/Ed25519) plus `agent_runtime_id` and registered `run_task_id`.
- `PersonalAccessTokenAuth`: PAT string.
- `BedrockApiKeyAuth`: Bedrock bearer token and target AWS region string.
- `BedrockAccessKeysAuth`: AWS `access_key_id`, `secret_access_key`, optional `session_token`, and region.

---

## 1.2 Credential Storage and Persistence (`auth.json` and Keyring)

Credentials are saved in `AuthDotJson` format, located at `$CODEX_HOME/auth.json` (falling back to `~/.codex/auth.json`).

### File Structure: `AuthDotJson`
Defined in `codex-rs/login/src/auth/storage.rs`:
```rust
#[derive(Deserialize, Serialize, Clone, Debug, PartialEq)]
pub struct AuthDotJson {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub auth_mode: Option<AuthMode>,

    #[serde(rename = "OPENAI_API_KEY")]
    pub openai_api_key: Option<String>,

    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub tokens: Option<TokenData>,

    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub last_refresh: Option<DateTime<Utc>>,

    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub agent_identity: Option<AgentIdentityStorage>,

    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub personal_access_token: Option<String>,

    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub bedrock_api_key: Option<BedrockApiKeyAuth>,

    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub bedrock_access_keys: Option<BedrockAccessKeysAuth>,
}
```

### Storage Backends
Codex abstracts credential storage via `AuthStorageBackend`:
1. **`FileAuthStorage`**:
   - Plaintext JSON written directly to `$CODEX_HOME/auth.json`.
   - File permissions are restricted to user-only (`0600` on Unix).
   - Atomic replacement via temporary file and atomic rename prevents half-written credential files.
2. **`KeyringAuthStorage`**:
   - Stores credentials inside the operating system keychain using `codex-keyring-store`.
   - macOS: Apple Keychain Services.
   - Linux: FreeDesktop Secret Service API over D-Bus.
   - Windows: Windows Credential Manager.
   - Stores a pointer/placeholder in `auth.json` while the secret tokens remain encrypted in the OS vault.
3. **`EphemeralAuthStorage`**:
   - In-memory only storage used by sub-agents, test harnesses, or sandboxed environments where persistent credential leakage must be prevented.

---

## 1.3 Interactive OAuth 2.0 PKCE Login Flow

When a user executes `codex login` or launches the CLI without pre-existing credentials, Codex initiates an OAuth 2.0 Authorization Code grant with **PKCE (Proof Key for Code Exchange)** using SHA-256 (`S256`).

Located in `codex-rs/login/src/server.rs` and `codex-rs/login/src/oauth/`:

```
User Terminal                     Local Browser                    Codex Login Server                OpenAI Auth Service
     |                                  |                           (127.0.0.1:1455)                (auth.openai.com)
     |                                  |                                  |                                |
     |-- Run `codex login` ------------>|                                  |                                |
     |                                  |                                  |-- Bind 127.0.0.1:1455 -------->|
     |                                  |                                  |   (Fallback to 1457 on in-use) |
     |                                  |                                  |-- Generate PKCE (S256) ------->|
     |                                  |                                  |-- Generate State (32 bytes) -->|
     |                                  |                                  |                                |
     |<-- Open Auth URL in Browser -----+----------------------------------|                                |
     |    (or print URL to terminal)    |                                  |                                |
     |                                  |-- GET /oauth/authorize ------------------------------------------>|
     |                                  |   (client_id, code_challenge,                                     |
     |                                  |    redirect_uri, state, scopes)                                   |
     |                                  |                                                                   |
     |                                  |   [User signs in, selects workspace, grants consent]              |
     |                                  |                                                                   |
     |                                  |<-- 302 Redirect to http://127.0.0.1:1455/auth/callback ----------|
     |                                  |    (?code=AUTH_CODE&state=STATE)                                  |
     |                                  |                                  |                                |
     |                                  |-- GET /auth/callback ----------->|                                |
     |                                  |                                  |-- Validate state ------------- |
     |                                  |                                  |-- POST /oauth/token ---------->|
     |                                  |                                  |   (code, code_verifier,        |
     |                                  |                                  |    grant_type=auth_code)       |
     |                                  |                                  |<-- ExchangedTokens ------------|
     |                                  |                                  |    (id_token, access_token,    |
     |                                  |                                  |     refresh_token)             |
     |                                  |                                  |                                |
     |                                  |                                  |-- Token Exchange (RFC 8693) -->|
     |                                  |                                  |   (exchange id_token for       |
     |                                  |                                  |    openai-api-key)             |
     |                                  |                                  |<-- API-Key Access Token -------|
     |                                  |                                  |                                |
     |                                  |                                  |-- Save to ~/.codex/auth.json ->|
     |                                  |<-- 200 OK HTML Success Page -----|                                |
     |                                  |                                  |-- Server gracefully exits ---->|
     |<-- "Signed in successfully" -----+----------------------------------+                                |
```

### Protocol Details:
- **Default Issuer**: `https://auth.openai.com`
- **Default Callback Port**: `1455` (Fallback: `1457`). If port 1455 is occupied by a stale Codex instance, a `/cancel` request is dispatched to gracefully shut down the zombie listener.
- **Client ID**: `app_EMoamEEZ73f0CkXaXp7hrann` (overrideable via `CODEX_APP_SERVER_LOGIN_CLIENT_ID`).
- **Scopes**: `openid profile email offline_access api.connectors.read api.connectors.invoke`.
- **Query Parameters**:
  - `response_type=code`
  - `client_id=app_EMoamEEZ73f0CkXaXp7hrann`
  - `redirect_uri=http://127.0.0.1:{actual_port}/auth/callback`
  - `code_challenge={S256_HASH}`
  - `code_challenge_method=S256`
  - `state={CRYPTO_RANDOM_BASE64}`
  - `id_token_add_organizations=true`
  - `codex_cli_simplified_flow=true`
  - `originator={originator}`
  - `allowed_workspace_id={workspace_id}` (optional constraint)

---

## 1.4 Token Exchange Flow (RFC 8693)

After receiving the initial tokens (`id_token`, `access_token`, `refresh_token`), Codex does not send the raw user OAuth access token directly to inference endpoints. Instead, it performs an RFC 8693 **OAuth 2.0 Token Exchange** (`obtain_api_key` in `codex-rs/login/src/server.rs`):

```http
POST /oauth/token HTTP/1.1
Host: auth.openai.com
Content-Type: application/x-www-form-urlencoded

grant_type=urn%3Aietf%3Aparams%3Aoauth%3Agrant-type%3Atoken-exchange
&client_id=app_EMoamEEZ73f0CkXaXp7hrann
&requested_token=openai-api-key
&subject_token={ID_TOKEN}
&subject_token_type=urn%3Aietf%3Aparams%3Aoauth%3Atoken-type%3Aid_token
```

The returned token is persisted and used as the bearer credential for ChatGPT-backed inference routes.

---

## 1.5 Token Refresh Architecture (Proactive and Reactive)

Codex implements a two-tier token refresh mechanism: proactive token rotation before requests are sent, and reactive recovery when an endpoint returns HTTP `401 Unauthorized`.

```
                        [ Request Triggered ]
                                  |
                                  v
                  +-------------------------------+
                  |  should_refresh_proactively?  |
                  +-------------------------------+
                     /                         \
             [YES]  /                           \  [NO]
                   v                             v
       +-----------------------+        +-------------------+
       | Guarded Disk Reload   |        | Dispatch Request  |
       +-----------------------+        +-------------------+
          /                 \                     |
   [Changed]               [No Change]            |
       /                       \                  v
      v                         v          +-------------+
[Adopt New Disk Tokens]   [POST /oauth/    | HTTP Status |
                           token Refresh]  +-------------+
                                |                 |
                                v          [401 Unauthorized]
                          [Save to Disk]          |
                                |                 v
                                v      +-----------------------+
                          [Dispatch]   | handle_unauthorized   |
                                       +-----------------------+
                                                  |
                                                  v
                                       +-----------------------+
                                       | Step 1: Reload Disk   |
                                       | (Check other process) |
                                       +-----------------------+
                                          /                 \
                                   [Changed]               [No Change]
                                       /                       \
                                      v                         v
                               [Retry Request]         +-----------------------+
                                                       | Step 2: Refresh Token |
                                                       | from Authority        |
                                                       +-----------------------+
                                                          /                 \
                                                   [Success]               [Permanent Error]
                                                      /                         \
                                                     v                           v
                                              [Retry Request]             [Cache Failure,
                                                                           Throw Fatal Error]
```

### 1. Proactive Refresh (`should_refresh_proactively`)
Located in `codex-rs/login/src/auth/manager.rs:3001`:
1. Decodes JWT claims in `access_token` and extracts `exp` timestamp.
2. If `expires_at <= Utc::now() + 5 minutes` (`CHATGPT_ACCESS_TOKEN_REFRESH_WINDOW_MINUTES`), refresh is triggered.
3. Fallback: If `last_refresh < Utc::now() - 8 days` (`TOKEN_REFRESH_INTERVAL`), refresh is triggered.
4. **Guarded Reload**: Before hitting the network, Codex executes `reload_if_account_id_matches()`. If another running Codex process or daemon has already refreshed the token on disk, Codex adopts the new on-disk token and avoids redundant network calls.

### 2. Reactive Refresh (`UnauthorizedRecovery`)
Located in `codex-rs/core/src/client.rs:2724` and `codex-rs/login/src/auth/manager.rs:1860`:
When an inference call or WebSocket handshake fails with `401 Unauthorized`:
1. **Provider Check**: If using Amazon Bedrock, invokes `provider.recover_from_unauthorized()` (re-runs AWS credential export or SSO profile refresh).
2. **AuthManager Recovery**:
   - **Step 1 (Reload)**: Re-reads `auth.json`. If the credential changed since the request was constructed, Codex immediately retries the request without refreshing.
   - **Step 2 (Refresh)**: Executes `refresh_token_from_authority_impl()`. Sends JSON payload to `https://auth.openai.com/oauth/token`:
     ```json
     {
       "grant_type": "refresh_token",
       "client_id": "app_EMoamEEZ73f0CkXaXp7hrann",
       "refresh_token": "..."
     }
     ```
   - Persists updated tokens and retries the turn.
3. **Permanent Failure Caching**: If the server returns an OAuth rejection (`invalid_grant`, `refresh_token_expired`, `refresh_token_reused`, `refresh_token_invalidated`), Codex registers a `RefreshTokenFailedError` in memory. Subsequent turns fail immediately without attempting further network calls until the user logs in again.

---

## 1.6 External Host Authentication

When Codex runs embedded inside an IDE (e.g. VS Code Extension) or via the App Server daemon (`codex-app-server`), auth credentials may be owned by the host application rather than local disk files.

Codex handles this via the `ExternalAuth` trait (`codex-rs/login/src/auth/manager.rs:275`):
- Mode: `AuthMode::ChatgptAuthTokens`
- When proactive or reactive refresh triggers, Codex dispatches a JSON-RPC notification `chatgptAuthTokens/refresh` to the host over stdout/IPC or WebSocket.
- The host returns refreshed tokens, which Codex validates against workspace restrictions and updates in memory.

---

## 1.7 Agent Identity Architecture

Codex implements cryptographic **Agent Identity** (`codex-rs/login/src/auth/agent_identity.rs` and `codex-rs/agent-identity`), enabling autonomous programmatic agents to sign their own API requests without sharing human user credentials.

### Mechanism:
1. **Key Generation**: Generates an RSA or Ed25519 PKCS#8 key pair locally.
2. **Registration**: Registers the agent runtime ID with the ChatGPT Agent Identity service (`register_managed_chatgpt_agent_identity`) using the human owner's access token.
3. **Task Registration**: Acquires a `run_task_id` bound to the agent session.
4. **Header Signing**: Every API request is signed on the fly using `authorization_header_for_agent_task()`:
   - Header: `Authorization: Bearer <signed_jwt>`
   - Header: `ChatGPT-Account-ID: <account_id>`
   - Header: `X-OpenAI-Fedramp: true` (if FedRAMP environment)
5. **Cooldown State**: Bootstrap attempts use exponential backoff (`MAX_AGENT_IDENTITY_BOOTSTRAP_ATTEMPTS = 3`) and a shared cooldown table to prevent cascading registration storms across concurrent child agents.

---

## 1.8 Workload Identity (Cloud and Machine Auth)

For unattended CI/CD, Kubernetes clusters, or headless cloud environments, Codex supports **Workload Identity Federation** (`codex-rs/login/src/auth/workload_identity.rs`):
- **Trigger**: Detected automatically when environment variables `OPENAI_FEDERATION_RULE_ID` and `OPENAI_IDENTITY_TOKEN_FILE` are set.
- **Assertion**: Reads the cloud provider's OIDC assertion token (from GitHub Actions, GCP Workload Identity, AWS IAM Roles for Service Accounts, etc.) from the specified file path.
- **Exchange**: Directly exchanges the assertion token at `https://auth.openai.com/oauth/token` for a Codex session token.
- **Singleton Process Registry**: Tracks workload identity fingerprints across threads to ensure no conflicting federation rules run concurrently in the same process.

---

## 1.9 Enterprise Gateway OAuth

When operating behind an enterprise egress proxy or specialized model gateway requiring secondary authentication, Codex supports **Gateway OAuth** (`codex-rs/login/src/gateway_auth.rs`):
- Specified in `model_providers.<id>.gateway_oauth` in `config.toml`.
- Configured with `authorization_url`, `token_url`, `client_id`, `scopes`, and delivery mechanism.
- Composed transparently with the primary inference credentials via `CombinedAuth`:
  - `Header`: Injects e.g. `Proxy-Authorization: Bearer <gateway_token>` or custom header.
  - `Cookie`: Injects `Cookie: <name>=<gateway_token>`.

---

## 1.10 Amazon Bedrock Authentication Matrix

Codex provides native support for Amazon Bedrock through 8 distinct authentication sources (`codex-rs/model-provider/src/amazon_bedrock/auth.rs`):

| Bedrock Auth Source | Credential Source | Signing / Transport |
|---|---|---|
| `CommandBearerToken` | CLI command defined in `auth.command` | HTTP `Authorization: Bearer <token>` |
| `ManagedBearerToken` | `BedrockApiKeyAuth` in `auth.json` | HTTP `Authorization: Bearer <token>` |
| `EnvBearerToken` | `AWS_BEARER_TOKEN_BEDROCK` / env var | HTTP `Authorization: Bearer <token>` |
| `CredentialExport` | CLI command in `aws.credential_export` | AWS SigV4 signed requests |
| `ConfiguredAwsProfile`| `~/.aws/config` profile via `aws.profile` | AWS SigV4 signed requests |
| `ManagedAccessKeys` | `BedrockAccessKeysAuth` in `auth.json` | AWS SigV4 signed requests |
| `EnvAwsCredentials` | `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | AWS SigV4 signed requests |
| `AwsSdk` | Default AWS SDK credential provider chain | AWS SigV4 signed requests |

---

# Part II: Codex API for Inference Architecture

## 2.1 Model Provider Abstraction and Wire Protocol

Inference abstraction is defined by the `ModelProvider` trait (`codex-rs/model-provider/src/provider.rs`):

```rust
pub trait ModelProvider: fmt::Debug + Send + Sync {
    fn info(&self) -> &ModelProviderInfo;
    fn auth(&self) -> ModelProviderFuture<'_, Option<CodexAuth>>;
    fn api_provider(&self) -> ModelProviderFuture<'_, Result<Provider>>;
    fn responses_api_provider<'a>(
        &'a self,
        routing_context: &'a WorkspaceRoutingContext,
    ) -> ModelProviderFuture<'a, Result<ResolvedResponsesProvider>>;
    fn api_auth_for_scope(
        &self,
        scope: ProviderAuthScope,
    ) -> ModelProviderFuture<'_, Result<ResolvedProviderAuth>>;
    fn models_manager(&self, cache_config: ModelsCacheConfig) -> Arc<dyn ModelsManager>;
    fn is_recoverable_auth_error(&self, error: &TransportError) -> bool;
    fn recover_from_unauthorized(&self) -> ModelProviderFuture<'_, Result<ProviderUnauthorizedRecovery>>;
}
```

### Protocol Standardization: WireApi::Responses
Codex has completely deprecated and removed legacy `/v1/chat/completions` support:
```rust
const CHAT_WIRE_API_REMOVED_ERROR: &str =
    "`wire_api = \"chat\"` is no longer supported.\n\
     How to fix: set `wire_api = \"responses\"` in your provider config.";
```
All model providers speak the **OpenAI Responses API** wire format (`/v1/responses` or WebSockets).

---

## 2.2 Workspace Routing and Managed Residency

For ChatGPT workspace users (Enterprise, Team, Edu), inference traffic is dynamically routed to tenant-specific or residency-specific clusters (`codex-rs/model-provider/src/workspace_routing.rs`):

1. **Origin Rewrite**:
   When `responses_api_provider` runs, Codex checks `auth_manager.workspace_routing()`. If the user belongs to an enterprise tenant with custom routing:
   - The provider `base_url` is mutated to point directly to the tenant's dedicated backend origin (e.g. `https://tenant-backend.openai.com`).
   - The header `x-openai-account-routing-override: us` or `us_cr` is injected.
2. **Redirect Policy Hardening**:
   Any workspace-routed endpoint immediately configures `ClientRedirectPolicy::Reject`. This prevents redirect attacks from bouncing authorized requests to unverified hosts.
3. **Managed Residency Enforcing**:
   If managed policies require US data residency (`ResidencyRequirement::Us`), Codex injects:
   ```http
   x-openai-internal-codex-residency: us
   ```

---

## 2.3 Model Discovery and Catalog Management (`/models`)

Codex discovers available models dynamically at startup and caches them (`codex-rs/model-provider/src/models_endpoint.rs`):
- **Direct OpenAI API Key**: Queries `https://chatgpt.com/backend-api/codex/models` (or `base_url`).
- **ChatGPT OAuth**: Queries `https://chatgpt.com/backend-api/codex/models` with the user's bearer token.
- **Amazon Bedrock**: Queries regional static catalog (`AMAZON_BEDROCK_GPT_5_5_MODEL_ID`, etc.) or remote Bedrock endpoints.
- **Model Metadata Returned**:
  - `slug`: Model identifier (e.g. `gpt-5-codex`, `o3-mini`).
  - `context_window` / `max_context_window`.
  - `default_reasoning_level` / `supported_reasoning_levels` (`low`, `medium`, `high`).
  - `service_tiers` (`fast`, `standard`, `flex`).
  - `use_responses_lite`: Whether to use Responses Lite payload wrapping.
  - `supports_reasoning_summary_parameter`.
  - `guardian`: Model-specific safety inspection policies.

---

## 2.4 Turn Pipeline and Prompt Construction

Every conversation turn is transformed into a single `ResponsesApiRequest` (`codex-rs/core/src/client.rs:951`):

1. **Input Normalization**:
   - `prompt.get_formatted_input_for_request(model_info)`: Normalizes images, sanitizes token limits.
2. **Deterministic Prefix Hashing**:
   - Computes deterministic UUIDs (v5 over namespace OID + thread ID) for base developer instructions and injected system prompts.
   - Prepends base instructions as `ResponseItem::Message` with role `"developer"`.
3. **Tool Definition Injection**:
   - Standard Responses API: Injects `tools: Some(ResponsesApiTools)` at the request body root.
   - Responses Lite: Injects tools into the prompt input array as `ResponseItem::AdditionalTools`.
4. **Reasoning Parameters**:
   - Sets `reasoning.effort` (`low`, `medium`, `high`, or custom integer tokens).
   - Sets `reasoning.summary` and `stream_options.reasoning_summary_delivery: SequentialCutoff`.
5. **Output Token Replay & Encryption**:
   - Injects `include: ["reasoning.encrypted_content"]`.
   - If output token replay is enabled: `include.push("output.encrypted_content")`.
6. **Prompt Cache Affinity Key**:
   - Injects `prompt_cache_key`: A deterministic session-bound key ensuring backend prompt caching hits across consecutive turns.

---

## 2.5 Request Header Assembly

Headers attached during inference requests:

| Header Name | Value / Format | Purpose |
|---|---|---|
| `Authorization` | `Bearer <token>` | Primary authentication (API Key, OAuth Access Token, Agent Identity JWT) |
| `ChatGPT-Account-ID` | `<workspace_uuid>` | Workspace tenant ID (ChatGPT OAuth & Agent Identity) |
| `X-OpenAI-Fedramp` | `"true"` | Routing through FedRAMP-certified enclave |
| `OpenAI-Beta` | `responses_websockets_v2` | Opts in to WebSocket Responses protocol on upgrade |
| `x-client-request-id` | `<thread_uuid>` | Idempotency and distributed log correlation |
| `session_id` | `<prompt_cache_key_or_session>` | Backend prompt cache affinity |
| `x-codex-routing-hint` | `model=<slug>;tier=<tier>` | Fast gateway routing hint before inspecting payload |
| `x-codex-turn-state` | Opaque state string | Turn state affinity token echoed back by backend |
| `x-oai-attestation` | `{"v":1,"s":0,"t":"..."}` | Hardware/Client device attestation token |
| `x-openai-subagent` | Subagent classification | Hierarchical agent trace labeling |
| `x-openai-account-routing-override` | `us` / `us_cr` | Tenant residency cluster routing |
| `x-openai-internal-codex-residency` | `us` | Enforced US data residency requirement |
| `OpenAI-Organization` | Configured Org ID | Multi-org project billing |
| `OpenAI-Project` | Configured Project ID | Scoped project routing |
| `version` | Codex package version | Client version tracking |

---

## 2.6 The Responses API Request Schema

Codex serializes requests to `/responses` using the following exact schema (`codex-rs/codex-api/src/common.rs:279`):

```json
{
  "model": "gpt-5-codex",
  "stream": true,
  "service_tier": "standard",
  "prompt_cache_key": "thread-c56a81...",
  "store": false,
  "tool_choice": "auto",
  "parallel_tool_calls": true,
  "reasoning": {
    "effort": "high",
    "summary": "auto",
    "context": "auto"
  },
  "stream_options": {
    "reasoning_summary_delivery": "sequential_cutoff"
  },
  "include": [
    "reasoning.encrypted_content",
    "output.encrypted_content"
  ],
  "text": {
    "verbosity": "medium",
    "format": {
      "type": "text"
    }
  },
  "client_metadata": {
    "session_id": "...",
    "thread_id": "...",
    "turn_id": "...",
    "parent_response_id": "resp_01..."
  },
  "tools": [
    {
      "type": "function",
      "name": "shell_command",
      "description": "Execute a command in the local sandbox shell.",
      "parameters": {
        "type": "object",
        "properties": {
          "command": { "type": "string" }
        },
        "required": ["command"]
      }
    }
  ],
  "input": [
    {
      "type": "message",
      "role": "developer",
      "content": [{ "type": "input_text", "text": "Base developer instructions..." }]
    },
    {
      "type": "message",
      "role": "user",
      "content": [{ "type": "input_text", "text": "Run tests and fix failures" }]
    }
  ]
}
```

---

## 2.7 Dual-Transport Architecture: WebSocket vs HTTP/SSE

Codex implements an intelligent transport fallback engine (`ModelClientSession::stream` in `codex-rs/core/src/client.rs:2321`):

```
+-----------------------------------------------------------------------------+
|                         ModelClientSession::stream                          |
+-----------------------------------------------------------------------------+
                                      |
                       Provider supports WebSockets?
                                 /         \
                         [YES]  /           \  [NO]
                               v             v
             +-----------------------+   +-----------------------+
             | Attempt WebSocket     |   | Stream via HTTP/SSE   |
             | Upgrade at /responses |   | POST /responses       |
             +-----------------------+   +-----------------------+
                         |
           Upgrade Successful (101 Switching)?
                     /             \
             [YES]  /               \  [NO: 426 Upgrade Required or Network Error]
                   v                 v
       +-----------------------+   +-----------------------+
       | Active WebSocket      |   | Fallback to HTTP/SSE  |
       | Connection            |   | (Cache fallback for   |
       +-----------------------+   |  session lifetime)    |
                                   +-----------------------+
```

### 1. Responses-over-WebSocket (`ApiWebSocketResponsesClient`)
- Upgrade URL: `wss://chatgpt.com/backend-api/codex/responses` (or `wss://api.openai.com/v1/responses`).
- Header: `OpenAI-Beta: responses_websockets_v2`.
- Transport: TCP + TLS + WebSocket framed over `tokio-tungstenite`.
- Keepalive: Managed background actor loop automatically answers `Ping` frames with `Pong`.
- Lifetime: Hard-capped at 60 minutes (`websocket_connection_limit_reached`); automatically reconnects when the threshold is approached.

### 2. HTTP Server-Sent Events Fallback (`ResponsesClient`)
- URL: `POST https://.../responses`.
- Header: `Accept: text/event-stream`.
- Body: JSON encoded, optionally compressed using **Zstandard (`zstd`)**.
- Stream parser: Decodes chunks via `eventsource_stream` with an idle timeout watchdog (`DEFAULT_STREAM_IDLE_TIMEOUT_MS = 300_000ms`).

---

## 2.8 Incremental WebSocket Turn Streaming

The primary advantage of Codex's WebSocket transport is **Turn Incrementalism** (`get_incremental_items` in `codex-rs/core/src/client.rs:1468`):

In standard HTTP requests, every single turn requires transmitting the full conversation history (often megabytes of accumulated shell outputs, tool results, and code). Over WebSocket:

```
Turn 1: Full History Transmitted
Client ---> ResponseCreateWsRequest { previous_response_id: None, input: [Msg1, Msg2, Tool1, ToolResult1] }
Server <--- Streams events, finishes with response_id: "resp_001"

Turn 2: Incremental Delta Only!
Client ---> ResponseCreateWsRequest {
              previous_response_id: Some("resp_001"),
              input: [UserMsg3]    <--- ONLY NEW ITEMS SENT!
            }
Server <--- Backend reconstructs context from "resp_001" and streams response!
```

### Invalidation & Recovery:
If the server loses backend state and returns error code `previous_response_not_found`, or if non-input request properties changed, Codex transparently resets the session cache and replays the full request.

---

## 2.9 Streaming Event Stream Processing and Parsing

Both WebSocket and SSE streams feed into `process_responses_event` (`codex-rs/codex-api/src/sse/responses.rs`), producing strongly typed `ResponseEvent` variants:

```rust
pub enum ResponseEvent {
    /// Server initialized response with unique response ID.
    Created { response_id: Option<String> },

    /// Content deltas:
    OutputTextDelta(String),
    ToolCallInputDelta { item_id: String, call_id: Option<String>, delta: String },
    ReasoningSummaryDelta { delta: String, summary_index: i64 },
    ReasoningContentDelta { delta: String, content_index: i64 },

    /// Structural items completed:
    OutputItemAdded(ResponseItem),
    OutputItemDone(ResponseItem),
    ReasoningSummaryDone { item_id: String, text: String, summary_index: i64 },

    /// Server control & metadata:
    ServerModel(String),                          // Actual model routed by safety proxy
    ServerReasoningIncluded(bool),                // Server accounted for reasoning tokens
    ModelVerifications(Vec<ModelVerification>),   // Step-up verification recommendations
    TurnModerationMetadata(TurnModerationMetadataEvent),
    SafetyBuffering(SafetyBuffering),             // Safety filtering triggered
    RateLimits(RateLimitSnapshot),                // Remaining tokens & requests
    ModelsEtag(String),

    /// Completion:
    Completed {
        response_id: String,
        token_usage: Option<TokenUsage>,
        usage_metadata: Option<ResponseUsageMetadata>,
        end_turn: Option<bool>,
    },
}
```

### Token Usage Accounting:
`TokenUsage` captures exact token metrics:
- `input_tokens`: Base prompt tokens.
- `output_tokens`: Model completion tokens.
- `cached_tokens`: Prompt tokens served from cache.
- `reasoning_tokens`: Tokens consumed during chain-of-thought reasoning.

---

## 2.10 Tool Execution, Turn Loop, and History Maintenance

The turn execution engine (`codex-rs/core/src/session/turn.rs`) drives the model turn loop:

```
+------------------------------------------------------------------------------------+
|                                    TURN LOOP                                       |
+------------------------------------------------------------------------------------+
  1. Submit Turn Request to ModelClientSession (WS or SSE)
  2. Stream ResponseEvents:
     - OutputTextDelta -> Rendered to TUI/Client
     - ToolCallInputDelta -> Buffered in argument accumulator
     - OutputItemDone(FunctionCall) -> Captured for dispatch
  3. On stream Completed:
     - Check function calls:
       * If NO function calls generated: Turn ends (`end_turn = true`). Complete Task!
       * If function calls generated:
         a. Evaluate security policies (ExecPolicy, Sandbox, User Approval prompts).
         b. Execute tools:
            - `shell_command`: Executed inside Linux bubblewrap / Windows sandbox.
            - `apply_patch`: Applies unified diff to workspace files.
            - `mcp_tool_call`: Dispatched to target MCP server over stdio/SSE.
            - `web_search`: Dispatched to web search provider.
         c. Package tool output as `ResponseItem::FunctionCallOutput`.
         d. Append tool outputs to conversation context.
         e. Loop to Step 1 (model inspects tool output and continues)!
```

---

## 2.11 Context Compaction and Context Window Management

When conversation history approaches the model's context window limit (`codex-rs/core/src/compact.rs` and `compact_remote_v2.rs`):

1. **Auto-Compaction Trigger**:
   - Monitored after every turn. When token estimate exceeds the compaction threshold (e.g. 80% of `context_window`):
2. **Execution**:
   - Runs a specialized inference turn (`RequestEffortUsage::Compaction`).
   - The model is instructed to generate a structured `CompactionSummary` preserving goals, file edits, test outcomes, and unresolved tasks.
3. **History Replacement**:
   - The old conversation items are pruned.
   - Inserted at the head: `ResponseItem::ContextCompaction` containing the compressed summary.
   - Retained: Active system instructions, world state, and the most recent turn items.

---

## 2.12 Error Recovery and Resilience State Machine

Codex handles runtime failures through a structured error-recovery hierarchy:

```
                                  [ Transport Error ]
                                           |
                   +-----------------------+-----------------------+
                   |                       |                       |
            [ 401 Unauthorized ]    [ 429 Rate Limit ]       [ 5xx / Network Drop ]
                   |                       |                       |
                   v                       v                       v
        handle_unauthorized()      Extract Retry-After     Exponential Backoff
                   |               header & emit           (base_delay: 200ms)
         +---------+---------+     rate limit events               |
         |                   |             |                       v
  [AWS Bedrock]       [AuthManager]        v             Stream Max Retries: 5
         |                   |       Sleep & Retry       Request Max Retries: 4
  Re-authenticate     Step 1: Reload                       |
  AWS SSO/Role        Step 2: Refresh                      v
         |                   |                   [ Exceeded Limits? ]
         +---------+---------+                         /        \
                   |                           [YES]  /          \  [NO]
                   v                                 v            v
            [ Retry Turn ]                     [ Fatal Error ]  [ Retry Turn ]
```

---

# Part III: Complete End-to-End Sequence Diagram

The following sequence diagram tracks the entire lifecycle of an authenticated Codex inference turn:

```
User               ThreadManager            ModelClientSession          AuthManager          WebSocket / SSE          OpenAI Backend
 |                      |                            |                       |                      |                           |
 |-- Submit Prompt ---->|                            |                       |                      |                           |
 |   "Run cargo test"   |                            |                       |                      |                           |
 |                      |-- current_client_setup --->|                       |                      |                           |
 |                      |   (routing=Workspace)      |-- auth() ------------>|                      |                           |
 |                      |                            |   (should_refresh?)   |                      |                           |
 |                      |                            |<-- Valid CodexAuth ---|                      |                           |
 |                      |                            |-- Resolve Routing --->|                      |                           |
 |                      |                            |   (origin, residency) |                      |                           |
 |                      |                            |                       |                      |                           |
 |                      |-- build_responses_request->|                       |                      |                           |
 |                      |   (Prompt, Tools, Headers, |                       |                      |                           |
 |                      |    Reasoning, Attestation) |                       |                      |                           |
 |                      |                            |                       |                      |                           |
 |                      |-- stream() --------------->|                       |                      |                           |
 |                      |                            |-- Connect WS /responses -------------------->|                           |
 |                      |                            |   (Headers: Auth, Account-ID,                |-- Upgrade Handshake ----->|
 |                      |                            |    Beta, Attestation, Residency)             |<-- 101 Switching Protocol-|
 |                      |                            |                                              |                           |
 |                      |                            |-- Send ResponseCreateWsRequest ------------->|                           |
 |                      |                            |   (incremental delta or full history)        |-- Dispatch to Model ----->|
 |                      |                            |                                              |                           |
 |                      |                            |<-- stream: response.created -----------------|                           |
 |                      |                            |<-- stream: response.output_text.delta -------|                           |
 |<-- Stream Text ------+----------------------------|    ("Running cargo test...")                 |                           |
 |                      |                            |<-- stream: response.tool_call_input.delta ---|                           |
 |                      |                            |<-- stream: response.output_item.done --------|                           |
 |                      |                            |    (FunctionCall: `shell_command`)           |                           |
 |                      |                            |<-- stream: response.completed ---------------|                           |
 |                      |                            |                                              |                           |
 |                      |-- Execute Tool ------------|                                              |                           |
 |                      |   (`cargo test`)           |                                              |                           |
 |                      |-- Tool Output Captured --->|                                              |                           |
 |                      |                            |                                              |                           |
 |                      |-- Next Turn in Loop ------>|                                              |                           |
 |                      |   (Attach Tool Result)     |-- Send Incremental ResponseCreateWsRequest ->|                           |
 |                      |                            |   (previous_response_id: "resp_123",         |                           |
 |                      |                            |    input: [ToolResult])                      |-- Model analyzes output ->|
 |                      |                            |<-- Streams model completion -----------------|                           |
 |<-- "Tests passed!" --+----------------------------|                                              |                           |
 v                      v                            v                                              v                           v
```

---

## Summary of Implementation Strengths

1. **Zero-Trust Token Management**: Refresh tokens are never sent to model inference routes; only scoped API keys or ephemeral agent-signed assertions are used.
2. **Defensive Multi-Process Concurrency**: Guarded reloads with shared semaphores prevent parallel processes or child agents from triggering duplicate token refreshes or invalidating refresh token rotations.
3. **Resilient Dual-Transport Architecture**: Transparently attempts high-performance incremental WebSockets and seamlessly degrades to streaming HTTP/SSE with Zstandard compression when proxies restrict WebSocket upgrades.
4. **Bandwidth & Latency Optimization**: Persistent WebSockets eliminate prompt re-transmission on subsequent turns by relying on backend response ID continuations.
5. **Comprehensive Provider Flexibility**: Standardized entirely on the Responses API wire protocol across OpenAI, Azure, Amazon Bedrock (Mantle and Runtime), and enterprise gateways.
