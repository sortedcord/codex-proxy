# API reference

## Authentication

Inference routes use `Authorization: Bearer $CODEX_PROXY_API_KEY` when configured.
Management routes **always** require `Authorization: Bearer $CODEX_PROXY_MANAGEMENT_KEY`,
including on loopback. Keys must be different. Missing management configuration
returns `503`; absent/wrong keys return `401`. No query-key authentication or CORS.

## Health

`GET /health` is public, reports `status: "ok"` and credential **file presence**, not
credential validity or upstream readiness. It never waits for refresh or calls OpenAI.
`/healthz` no longer serves health. Unsupported methods return `405`.

## Inference

- `GET /v1/models`: OpenAI-style model list, with unprefixed IDs.
- `GET /v1/models?client_version=...`: native Codex catalog.
- `POST /v1/responses`: `stream:false` returns JSON; `stream:true` returns SSE.
- `POST /v1/responses/compact`: upstream passthrough when available; streaming rejected.

Aliases without `/v1` and under `/backend-api/codex` apply only to inference.
Without usable credentials, model/generation calls fail locally with `503` before
reading the inference body. Unsupported developer-API state features return `400`.

Requests preserve native tools, images, reasoning, text formats and unknown fields.
String input becomes a user message; system messages become developer messages.
Empty function arguments become `{}`. Instructions default to empty; parallel tool
calls default true only when absent; encrypted reasoning is included for full replay.
Upstream always receives `stream:true` and `store:false`.

Metadata is removed upstream and echoed in response envelopes, not persisted. The
usage streaming hint is removed; other native stream options survive. Unsupported
sampling/limits (`temperature`, `top_p`, `max_output_tokens`, `max_completion_tokens`,
`truncation`), `user`, retention/cache options, context management and cache-breakpoint
hints are removed. The `fast` service tier maps to `priority`. These removals do not
implement equivalent sampling or output caps.

Send full prior native output and tool results for each turn. No `previous_response_id`,
conversation ID, stored prompt reference, background operation, or stored-response
retrieval/cancellation/deletion. SSE completion fills missing terminal output from
completed native items, never guessed delta concatenation. HTTP errors and safe retry/
request/turn-state headers are forwarded. In-band `error` frames remain native; collected
errors map known request/context problems to `400`, authentication to `401`, quota to
`429`, overload to `503`, and unknown failures to `502`. There is no inference retry
once streaming has begun.

## Account status

`GET /status/account` returns non-secret profile fields:

```json
{
  "credential_configured": true,
  "account_id": "example-account",
  "email": "user@example.invalid",
  "name": "Example User",
  "identity_source": "token_claims_unverified",
  "plan_type": "plus",
  "plan_source": "wham/usage",
  "quota_available": true,
  "quota_stale": false
}
```

Optional absent fields are omitted. Identity/initial plan claims are decoded locally,
not signature-verified. Plan metadata from a quota observation supersedes the claim
hint and identifies its source; stale data stays marked stale. This is not independent
billing verification. Missing/unusable account credentials return `503`.

## Quota status

`GET /status/limit` reads the latest persisted observation for the active account;
it does not synchronously contact OpenAI. It includes:

- `observed_at`, `account_id`, `source`, optional hashed `identity_key`, `plan_type` and `credits`;
  the identity key separates users in the same workspace without exposing their user IDs;
- `pools` keyed by provider metered-feature ID (`codex` plus reported extras);
- each pool's `allowed`, `limit_reached`, nullable `primary` / `secondary` windows;
- reported `used_percent`, derived `remaining_percent`, `window_seconds`, nullable
  `reset_at`; missing windows are null, not fabricated five-hour/weekly allowances;
- `polling.interval_seconds`, `last_attempt_at`, `last_success_at`, sanitized error;
- `stale`, set on poll failure or after two configured polling intervals.

Before any durable observation, it returns `503` with `quota_unavailable` and polling
metadata. Subsequent failures retain the last known snapshot with `stale:true`.
The source is Codex's `/backend-api/wham/usage`, not local per-request counters.

## Persistent history

`GET /status/usage` queries only the active account's journal.

| Query | Values / default |
| --- | --- |
| `from`, `to` | Optional inclusive RFC3339 timestamps with timezone |
| `pool` | `all` (default) or reported pool ID such as `codex` |
| `window` | `all` (default), `primary`, `secondary` |
| `limit` | `1`–`1000`, default `100` |
| `offset` | Non-negative integer, default `0` |
| `order` | `asc` or `desc` (default), by observation timestamp |

Unknown/duplicate parameters and reversed date ranges return `400`. The JSON contains
`samples: []`, filtered `total`, `limit`, `offset`, `order`, and polling metadata.
Offset pagination describes the retained history, not an immutable export cursor.
Observations record provider quota snapshots, **not exact per-request consumption**.

## Login and account replacement

`GET /config/login` begins or reuses one pending attempt and returns `login_url`,
`expires_at`, and `status: "pending"`. Open the URL in a browser. It binds a temporary
loopback callback on an OpenAI-allowlisted Codex port (`1455`, fallback `1457`), using
PKCE S256 and random state. Attempts expire after five minutes.

`POST /config/login` accepts either:

```json
{"url":"http://127.0.0.1:1455/auth/callback?code=...&state=..."}
```

or:

```json
{"code":"one-time-code","state":"state-from-the-login-attempt"}
```

Do not mix forms. URLs must match the pending callback origin/path; duplicate code/
state parameters and foreign URLs are rejected. Uncompressed JSON is bounded to
64 KiB and must contain one object with only the documented fields. No token upload.

Success returns `status: "configured"` and non-secret profile fields. It replaces the
single active account immediately without restarting, only after candidate history
preparation and credential persistence. Failed preparation/save leaves the previous
account and credentials intact. A post-rename directory-sync error returns `500` with
`credential_configured:true` and `durability_uncertain:true`: the replacement actually
occurred, so runtime follows disk instead of falsely claiming rollback.

Invalid state/callback returns `400`, expired attempts `410`, concurrent completion/
replay `409`, OAuth exchange failures `502`, local activation failures `500`, and
closed/disabled management `503`. Abandoned, failed, consumed and expired attempts
release callback listeners. Begin a fresh GET after a terminal attempt; codes are not
retried. Never expose management keys, codes or redirect URLs in request logs.

Existing streams can finish with their original account. A rejected old-account request
cannot recover under the replacement's credentials. Start new conversations after
switching; encrypted reasoning is account-specific. External file account changes are
not silently adopted: restart or authenticate explicitly through runtime login.
