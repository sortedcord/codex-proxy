# Architecture

The organization follows `sortedcord/antigravity-proxy`, but protocol/provider logic
remains Codex-specific and standard-library-only:

- `cmd/codex-proxy`: CLI parsing, signals, account and key management commands, exit behavior.
- `internal/apikey`: key generation, hashing (`sha256:`), extraction, and constant-time registry lookup.
- `internal/config`: flags, environment, multi-account schema, strict JSON decoding, validation, and persistence.
- `internal/oauth`: native Codex auth documents, per-account managers, serialized refresh, identity hints,
  multi-session PKCE browser flows, and atomic account replacement.
- `internal/proxy`: runtime ownership, multi-account key dispatch, admin API, Responses transport,
  request normalization, terminal output repair, streaming metadata and idle watchdog.
- `internal/quota`: Codex usage requests and normalization of provider-reported limit
  families/windows. No local estimation or Google model-pool assumptions.
- `internal/status`: independent polling, cached read APIs, private bounded journals per account,
  account attribution, staleness and historical filtering.
- `internal/storage`: regular-file checks, private atomic writes and native OS locks.
- `internal/httputil`: bounded reads and shared authentication/HTTP helpers.

## Multi-Account Architecture

The proxy supports hosting multiple ChatGPT/Codex accounts concurrently within one server process:

1. **Account-Scoped Key Dispatch**:
   `apikey.Registry` maps client API key hashes (`cxp_...`) directly to `{AccountID, KeyID}` bindings in constant time.
   Incoming inference and status requests are routed to the matching account runtime.
   On loopback interfaces with exactly one configured account and zero keys, unauthenticated calls fall back
   to that account for local development convenience.

2. **Per-Account Token Refresh and State**:
   Each account owns its own `oauth.Manager` and token state. Token refreshes are mutex-serialized per account,
   and rotated credentials are durably written to `config.json` via atomic file replacements, leaving legacy
   `auth.json` files untouched.

3. **Isolated Status & Quota Journals**:
   Quota observations are written to private, hashed per-account journals (`usage-<sha256>.jsonl`) protected
   by OS locks (`.lock`). Polling runs independently per account.

4. **Admin API Protected by ADMIN_API_KEY**:
   Management routes (`/admin/*`) require `ADMIN_API_KEY`. The Admin API provides full CRUD for accounts,
   API key generation and revocation, login session creation and completion, and aggregated quota monitoring.

## Inference fidelity

Only the necessary Codex compatibility fields are normalized. Native completed output
items and raw JSON retain numeric precision and unknown extension fields. SSE preserves
frames except response metadata and missing terminal output; it does not fabricate
messages from delta text. Non-streaming collects the same native terminal semantics.
Both modes use one bounded SSE frame parser. Native terminal events (including errors)
end streaming immediately, without another network read or optional legacy `[DONE]`.
Premature EOF aborts the downstream connection rather than reporting successful completion.

The downstream inference key is never an upstream token. The proxy selects the account
credential itself, ignores client cookies/account overrides, and forwards only safe
headers. Authentication recovery is bounded to three attempts and one reactive refresh;
no inference replay occurs after streaming starts.

## Login commit boundary

There is one active native auth document, not an account scheduler. A login attempt owns
random state, a PKCE verifier, a temporary allowlisted callback listener and an expiry
timer. A code is exchanged at most once. Only tokens returned from the trusted OAuth
endpoint can reach activation; the API does not accept token uploads.

Activation validates identity and prepares that account's journal **before** saving.
The auth mutex serializes refresh and replacement. Save commits a synced private file
and then updates the anchored account before releasing the mutex. A post-rename sync
failure publishes the disk account with explicit durability uncertainty, not fake
rollback. Pre-commit failure keeps the old account unchanged.

Each request obtains a credential snapshot. Recovery compares the rejected snapshot's
account ID and login generation before adopting newer tokens. Even a fresh login into
the same workspace cannot replay an old request under replacement credentials. External file account replacement
is not an implicit authorized hot-switch. Inference binds that snapshot before reading
the request body and avoids a second credential read unless refresh is needed.
Bounded credential bytes are checked by content hash, not mtime/size; cached parsing
cannot hide same-size external rotation. Returned auth maps do not alias the cache.
Unchanged non-secret profile snapshots can be read without waiting for refresh I/O.

## Quota/history lifecycle

Management is opt-in through a distinct key. Enabling it starts one bounded polling
worker and the login service. HTTP status reads cached, durable observations; it never
performs quota network calls synchronously. Quota failures cannot prevent ordinary
inference or cause fabricated available capacity.

Observations are attributed to the credentials that fetched them, including a response
arriving after an account switch. Each hashed journal verifies record identity (workspace plus reported user identity),
uses private files and a lifetime lock, syncs appends, and compacts retained samples by
atomic replacement. API-visible samples always obey the configured retention count.
The physical journal allows up to 25% superseded-record slack (at least one record)
between compactions and is always bounded to 16 MiB; records are bounded to 64 KiB.
Byte-pressure compaction targets 80% of the cap for append headroom, using exact
record sizes instead of estimates. Startup restores only the newest logical samples,
and discards only an incomplete final line; complete malformed records fail closed.
Post-rename sync failures publish the disk state in memory and stop further writes.
Pagination filters the retained snapshots before sorting/counting.

At most two account histories are cached. Older accounts remain on disk and can be
reopened on an explicit switch back. If the active identity is unavailable, opening a
third history fails without eviction. Account callbacks run outside the cache lock.
Runtime shutdown cancels/joins polling and login,
waits for independent bounded credential refresh, closes transports and releases locks.

## Verification boundaries

Unit/race tests cover fake-provider OAuth, state/PKCE, browser/manual callbacks, account
replacement and failure preservation, old-request recovery isolation, persisted history,
quota parsing and errors. A live read-only account test verifies the real usage endpoint.
Real inference tests verify native Codex CLI and Bifrost behavior. Automated tests never
perform a real browser login or hot-switch the installed account; that requires the
administrator's interactive authorization.

Bifrost dependencies live in a nested test-only module, not the production module.
Provider references/specifications are retained under `docs/reference/`; ignored source
checkouts under `reference-projects/` are not runtime dependencies.
