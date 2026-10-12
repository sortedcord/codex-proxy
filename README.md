# codex-proxy

A small, single-account HTTP proxy from the **OpenAI Responses API** to the
**ChatGPT Codex backend**. The runtime uses only Go's standard library: no
third-party dependencies, provider framework, database, or admin interface.

It reuses a file-backed `codex login` session. It does **not** proxy the ChatGPT
website or route to the paid OpenAI developer API.

## Quick start

Requires **Go 1.22+** and Codex authenticated with a ChatGPT account:

```sh
codex login                           # only if not already authenticated
mkdir -p bin
go build -trimpath -ldflags='-s -w' -o bin/codex-proxy .
./bin/codex-proxy                     # 127.0.0.1:8080
```

Credentials come from `$CODEX_HOME/auth.json`, or `~/.codex/auth.json`. The
credential file and its parent directory must be writable for token rotation.
No credentials are copied into this repository.

```sh
curl http://127.0.0.1:8080/v1/models

curl http://127.0.0.1:8080/v1/responses \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-5.5","input":"Reply with hello.","stream":false}'

curl -N http://127.0.0.1:8080/v1/responses \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-5.5","input":"Reply with hello.","stream":true}'
```

Choose a model returned by `/v1/models`; availability depends on your account.

### Official Codex CLI

Add the following provider to your Codex configuration, or use the snippet in
[`examples/codex.config.toml`](examples/codex.config.toml):

```toml
model_provider = "codex_proxy"
model = "gpt-5.5"

[model_providers.codex_proxy]
name = "codex-proxy"
base_url = "http://127.0.0.1:8080/v1"
wire_api = "responses"
requires_openai_auth = false
supports_websockets = false
# env_key = "CODEX_PROXY_API_KEY"       # enable if the proxy requires a local key
```

`supports_websockets = false` is intentional. Custom providers use uncompressed
HTTP/SSE; the proxy does not accept Zstandard request bodies. The CLI still owns
its agent loop and tool execution; the proxy only transports inference requests.

### OpenAI SDK

```python
import os
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:8080/v1",
    api_key=os.environ.get("CODEX_PROXY_API_KEY", "local"),
)
response = client.responses.create(model="gpt-5.5", input="Reply with hello.")
print(response.output_text)
```

The SDK's `api_key` authenticates to **this proxy**, not OpenAI. The proxy never
forwards it upstream. Installing the SDK is optional and not a proxy dependency.

### Bifrost OpenAI provider

Use an **OpenAI-based custom provider** named `codex-proxy`. The ready-to-merge
configuration is [`examples/bifrost.config.json`](examples/bifrost.config.json),
based on Bifrost's [OpenAI provider documentation](https://docs.getbifrost.ai/providers/supported-providers/openai#openai)
and verified against its real **core v1.11.3** provider.

1. Set the same `CODEX_PROXY_API_KEY` environment variable in both processes.
   This is a local proxy key, **not** a Codex OAuth token or an OpenAI API key.
2. Run `./bin/codex-proxy -addr 127.0.0.1:8081`. Port `8081` avoids Bifrost's
   default gateway port, `8080`.
3. Merge the example's provider into Bifrost's configuration. Set its base URL to
   `http://127.0.0.1:8081`, **without `/v1`**: Bifrost appends `/v1/responses`.
   Enable `disable_store`; permit only `responses`, `responses_stream`, and
   `list_models`. Supplied `allowed_requests` flags use an allowlist, so omitted
   operations stay disabled. Automatic provider retries are disabled.
4. Call Bifrost using the provider-prefixed model name:

```sh
curl http://127.0.0.1:8080/openai/v1/responses \
  -H 'Content-Type: application/json' \
  -d '{"model":"codex-proxy/gpt-5.5","input":"Reply with hello.","stream":false}'
```

Add your gateway's authentication header if Bifrost requires one; it is separate
from the proxy key. Use `stream:true` with `curl -N` for SSE. In the OpenAI SDK,
the Bifrost-facing base URL is `http://127.0.0.1:8080/openai/v1`, not the proxy URL.

If Bifrost runs in Docker or on another host, loopback points at **Bifrost's own
network namespace**. Set a reachable proxy address and configure Bifrost's
`network_config.allow_private_network: true` for private-address connections.
A non-loopback proxy listener requires a local key; protect remote access with
TLS and a firewall. Never give Bifrost the Codex credential file.

Verified: model discovery, text and image inputs, structured JSON output,
metadata, streaming terminal output, function calls with full-history replay,
and HTTP/in-band errors. This is Responses wire compatibility, **not every
OpenAI feature**: background jobs, stored-response lifecycle, conversation IDs,
stored prompt references, and unsupported Codex sampling/limits are unavailable.
Compaction remains upstream-dependent and is deliberately not enabled in the
Bifrost example. See the compatibility notes below.

## HTTP surface

| Method | Route | Behavior |
| --- | --- | --- |
| GET | `/healthz` | Process liveness; does not call OpenAI or validate credentials |
| GET | `/v1/models` | OpenAI-style model list, fetched from the account's live catalog |
| GET | `/v1/models?client_version=0.162.1` | Native Codex catalog, including model capabilities |
| POST | `/v1/responses` | Native Responses JSON; SSE when `stream:true`, JSON otherwise |
| POST | `/v1/responses/compact` | JSON passthrough to the upstream compaction endpoint, if available |

The same model and Responses routes are accepted under `/backend-api/codex`
and without a prefix. All other routes return `404`; unsupported methods return
`405`. Response retrieval/deletion by ID is not supported.

**Compaction availability:** local integration tests cover compaction forwarding.
The live upstream on the development machine returned `404` for
`/responses/compact`. That response is propagated unchanged; the proxy does not
pretend to implement compaction locally.

### Responses compatibility

The subscription backend is not a drop-in implementation of every developer API
feature. The proxy performs only the compatibility adjustments needed here:

- Sends `stream:true` and `store:false` upstream. Non-streaming requests collect
  the terminal response. In **both modes**, omitted terminal output is rebuilt
  from completed native SSE items in `output_index` order. Tool calls, encrypted
  reasoning, usage, and extension fields are retained; native failed/incomplete
  terminal responses retain their status. Deltas, event IDs, retry fields,
  comments, and sequence numbers are not synthesized.
- Converts string input into a user message, `system` messages into `developer`
  messages, and empty string function arguments into `"{}"`.
- Defaults `instructions` to an empty string, defaults `parallel_tool_calls` to
  true only when absent, and adds `reasoning.encrypted_content` to `include`.
- Removes unsupported sampling/output limits: `temperature`, `top_p`,
  `max_output_tokens`, `max_completion_tokens`, and `truncation`. **Do not rely on
  these parameters to cap output or control sampling.**
- Removes `user`, `prompt_cache_options`, `prompt_cache_retention`,
  `context_management`, and input `prompt_cache_breakpoint` hints. Maps the `fast`
  service-tier alias to `priority`; other tiers are left for upstream validation.
- Keeps request `metadata` locally and echoes it in JSON responses and streamed
  response envelopes; Codex rejects that parameter. Metadata is not persisted.
  Removes `stream_options.include_usage`: native terminal events already include
  usage. Other native stream options are preserved.
- Rejects background execution, non-null `previous_response_id`, `conversation`,
  and stored `prompt` references. For multi-turn conversations, send full history,
  including prior output items and tool results. No server-side conversation or
  replay cache is maintained.
- Preserves upstream HTTP errors, retry hints, request IDs, and Codex turn-state
  headers. Streaming in-band errors pass through unchanged; collected in-band
  errors become JSON errors (`400` for known request/context errors, `401` for
  authentication errors, `429` for quota errors, `503` for overload, `502`
  otherwise). Proxy-generated errors use standard OpenAI error types. Streams
  are never replayed after delivery starts.

No chat completions, Anthropic/Gemini translation, account pooling, load
balancing, automatic tool execution, WebSockets, realtime/audio, search-specific
routes, browser login UI, keyring integration, or API-key/enterprise/Bedrock
credential modes are included.

## Configuration

Flags override their corresponding environment defaults.

| Flag | Environment | Default |
| --- | --- | --- |
| `-addr` | `CODEX_PROXY_ADDR` | `127.0.0.1:8080` |
| `-auth-file` | `CODEX_PROXY_AUTH_FILE` | `$CODEX_HOME/auth.json` or `~/.codex/auth.json` |
| `-upstream` | `CODEX_PROXY_UPSTREAM` | `https://chatgpt.com/backend-api/codex` |
| `-client-version` | `CODEX_PROXY_CLIENT_VERSION` | `0.162.1` |
| `-idle-timeout` | — | `5m` |
| — | `CODEX_PROXY_USER_AGENT` | `codex_cli_rs/<version> (<OS>; <architecture>)` |
| — | `CODEX_PROXY_API_KEY` | Empty; no downstream key required on loopback |

`-upstream` is an operator-controlled trust boundary: the configured server
receives your access token. Only HTTPS is allowed, except HTTP on loopback for
local testing. Redirects are never followed. Ordinary `HTTP_PROXY`, `HTTPS_PROXY`,
and `NO_PROXY` environment variables are honored by Go's transport.

### Authentication and safety

- OAuth uses `tokens.access_token` as the bearer token and the stored account ID
  as `Chatgpt-Account-Id`. An ID-token claim can supply a missing account ID;
  decoding claims is not signature verification.
- Credentials are reloaded before each request. Access tokens are refreshed
  within five minutes of JWT expiry, with an eight-day `last_refresh` fallback
  for opaque tokens. A `401` first adopts a changed disk token, otherwise refreshes
  once. Recovery is bounded to three inference attempts and at most one reactive
  refresh per request. Other errors are left to the caller; no automatic retries
  of inference on `429`/`5xx` or ambiguous network failures.
- Refreshes are serialized within the process, bounded to 30 seconds, and finish
  even if the requesting client disconnects. Fatal refresh failures are cached
  until credentials change. Responses from the token endpoint are never logged.
- Rotated credentials preserve unknown fields and are written via a synced,
  `0600` temporary file and atomic rename. Before saving, the proxy checks for an
  external rotation, logout, or account switch. An account change requires a
  proxy restart.
- **Run only one token-refreshing process per auth file.** Official Codex does
  not participate in this proxy's synchronization; disk checks cannot eliminate
  cross-process OAuth rotation races. Codex clients configured with this custom
  provider do not need to manage upstream authentication themselves. Do not run
  another proxy or a directly authenticated Codex session against the same file
  concurrently. Copying refresh tokens does not solve the race.
- Binding beyond loopback is refused unless `CODEX_PROXY_API_KEY` is set. Remote
  deployment also requires a TLS reverse proxy and firewall. There is no built-in
  TLS listener or CORS support. Any permitted local user/client can consume the
  account's quota when a downstream key is not configured.
- Authorization, cookies, account overrides, and hop-by-hop headers from clients
  are not forwarded. Logging excludes prompts, responses, and token values.
- Request bodies are capped at 16 MiB; buffered JSON responses and retained
  completed output at 32 MiB; SSE frames at 16 MiB. Streaming uses bounded frame
  buffers and a 32 KiB copy buffer, immediate flushing, disconnect cancellation,
  and an idle watchdog rather than a whole-turn timeout.
  Header/body ingestion and blocked downstream writes have bounded deadlines.
- `SIGINT`/`SIGTERM` drain requests for up to ten seconds, then close connections.
  Shutdown also waits for any in-flight OAuth refresh (bounded to 30 seconds) so
  a rotated token is not abandoned before persistence.

For a protected listener, generate a local key outside the repository, export it
as `CODEX_PROXY_API_KEY`, and give clients the same environment variable. Never
put real keys or Codex credential files in version control.

## Tests

Normal tests use only `httptest` and synthetic credentials. They never read your
real authentication file or contact OpenAI:

```sh
go test -race -cover ./...
go vet ./...
go test -run '^$' -fuzz FuzzPrepareRequest -fuzztime 5s
go test -run '^$' -fuzz FuzzCollectResponse -fuzztime 5s
go test -run '^$' -fuzz FuzzMetadataStream -fuzztime 5s
```

The actual Bifrost provider is pinned in a **separate test-only module**, so it
adds no runtime dependencies and does not change the proxy's Go 1.22 minimum.
Its current SDK requires **Go 1.27+**. CI runs this test with a fake Codex backend:

```sh
CODEX_PROXY_BIFROST_TEST=1 \
  go test -race -run '^TestBifrostCompatibility$' -v -timeout 10m
```

Opt-in live tests use the installed credentials and consume account quota:

```sh
CODEX_PROXY_LIVE_TEST=1 go test -run TestLiveProxy -v -timeout 5m

# Also exercise the installed official Codex CLI without rewriting its config:
CODEX_PROXY_LIVE_TEST=1 CODEX_PROXY_TEST_CODEX=1 \
  go test -run TestLiveProxy -v -timeout 5m
```

To run model discovery, text/image input, structured JSON, metadata, streaming,
and a tool round trip through **Bifrost's real provider and real Codex**:

```sh
CODEX_PROXY_BIFROST_LIVE_TEST=1 \
  go test -race -run '^TestBifrostLive$' -v -timeout 10m
```

Set `CODEX_PROXY_TEST_MODEL` to override the live tests' `gpt-5.5` default.
Development verification covered live model discovery, both response modes,
a full-history function-call/result round trip, and official `codex exec` over
HTTP/SSE. Token rotation/failure, redirects, credential/header isolation,
compaction passthrough, disconnect cancellation, limits, and idle timeouts are
covered by local tests rather than deliberately invalidating live credentials.

## Layout

- `main.go`, `config.go`: startup, flags, transport, graceful shutdown.
- `auth.go`: Codex credential loading, refresh, and persistence.
- `proxy.go`: narrow routing and upstream/downstream credential boundaries.
- `request.go`: minimal Codex request normalization.
- `sse.go`, `response_output.go`: SSE collection and bounded native output repair.
- `response_stream.go`, `idle_reader.go`: SSE framing, metadata, and stream liveness.
- `integration/bifrost/`: isolated conformance tests against Bifrost's OpenAI provider.

Reference project checkouts, binaries, authentication files, and runtime state
are intentionally ignored by Git.
