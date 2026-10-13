# codex-proxy

A small, single-active-account proxy from the **OpenAI Responses API** to the
**ChatGPT Codex backend**. The runtime uses only Go's standard library: no
third-party dependencies, database, admin UI, or cross-provider translation.

It reuses file-backed `codex login` credentials. Optional management APIs provide
account/quota status, persistent quota history, browser OAuth login, and account
replacement without restarting. It does not route to the paid OpenAI developer API.

## Quick start

Requires **Go 1.22+**:

```sh
codex login                           # optional if using runtime login below
./scripts/build.sh
./bin/codex-proxy version
./bin/codex-proxy serve               # 127.0.0.1:8080; bare invocation also works
```

Credentials come from `$CODEX_HOME/auth.json`, otherwise `~/.codex/auth.json`.
The credential directory must be writable for refresh, the process lock, and login.
The server can start with no credential file; inference then returns `503` until
credentials are configured. Malformed or unsupported existing credentials fail startup.

```sh
curl http://127.0.0.1:8080/health
curl http://127.0.0.1:8080/v1/models
curl http://127.0.0.1:8080/v1/responses \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-5.5","input":"Reply with hello.","stream":false}'
```

Choose a model returned by `/v1/models`. Use `stream:true` and `curl -N` for SSE.
If an inference key is configured, add `Authorization: Bearer $CODEX_PROXY_API_KEY`
to model and Responses requests. The proxy exposes **unprefixed** model IDs.

### Docker

The [Docker image workflow](.github/workflows/docker.yml) publishes images to
**`ghcr.io/sortedcord/codex-proxy`** for `linux/amd64` and `linux/arm64`, after Go
formatting, vet, and race tests pass:

```sh
docker pull ghcr.io/sortedcord/codex-proxy:latest
```

Pushes to `main` publish `main`, `latest`, and `sha-<full-commit>` tags. Version
pushes such as `v1.2.3` publish `1.2.3`, `1.2`, and `1` tags (stable releases also
update `latest`). Pull requests targeting `main` build both platforms without
logging in or publishing. The workflow also supports manual runs and uses the
built-in `GITHUB_TOKEN`; no separate registry secret is required. For anonymous
pulls, set the GHCR package visibility to **public** after its first publication.

To build locally, a multi-stage [Dockerfile](Dockerfile) produces a non-root
`scratch` image containing only the static binary and CA certificates:

```sh
docker build -t codex-proxy .
```

The image listens on `0.0.0.0:8080`, so **`CODEX_PROXY_API_KEY` is required**.
See [Docker deployment](docs/deployment.md#docker) for persistent writable credential/
history mounts, a read-only root filesystem, and container OAuth login.

## Versioning

The current baseline is **v0.5.0**. During stabilization, feature releases use
`v0.x.0` and fixes/small updates increment the patch within that series (for
example, `v0.5.1`). The maintainer decides when to promote the project to v1;
from then on, normal SemVer compatibility rules apply.

Run `codex-proxy version` to print the executable version, commit, and date
without loading credentials or starting the server. Local builds report
`v0.5.0-dev`; tagged builds can inject release identity. This version is separate
from the upstream Codex client version. See [versioning and build metadata](docs/versioning.md).

## Optional management

Set a **separate** management key to enable all management APIs, quota polling,
and history. Without it, those APIs return `503`, and there is no background quota
polling or history initialization. Never give this key to inference clients.

```sh
# Generate and store keys outside this repository. OpenSSL is only an example.
export CODEX_PROXY_API_KEY="$(openssl rand -hex 32)"
export CODEX_PROXY_MANAGEMENT_KEY="$(openssl rand -hex 32)"
./bin/codex-proxy serve
```

From another shell with the management key:

```sh
curl http://127.0.0.1:8080/status/account \
  -H "Authorization: Bearer $CODEX_PROXY_MANAGEMENT_KEY"
curl http://127.0.0.1:8080/status/limit \
  -H "Authorization: Bearer $CODEX_PROXY_MANAGEMENT_KEY"
curl 'http://127.0.0.1:8080/status/usage?pool=codex&window=primary&limit=20' \
  -H "Authorization: Bearer $CODEX_PROXY_MANAGEMENT_KEY"
```

Quota windows, percentages and reset times come from Codex's account usage endpoint,
not token-based estimates. Missing windows stay unknown; five-hour/weekly periods are
not assumed. Cached status includes observation timestamps and staleness. History
records quota snapshots, **not a billing ledger or exact per-request consumption**.

### Runtime browser login and hot-switching

```sh
curl http://127.0.0.1:8080/config/login \
  -H "Authorization: Bearer $CODEX_PROXY_MANAGEMENT_KEY"
```

Open the returned `login_url`. OAuth uses Codex's client ID, PKCE, random state,
a five-minute attempt lifetime, and a temporary **loopback-only** callback on port
`1455` or `1457`. Completing it saves native Codex credentials and activates the
account. Repeating this flow replaces the active account without restarting.

For a remote server/container whose loopback callback your browser cannot reach,
copy the final redirect URL from the browser and submit it:

```sh
curl http://127.0.0.1:8080/config/login \
  -H "Authorization: Bearer $CODEX_PROXY_MANAGEMENT_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"url":"http://127.0.0.1:1455/auth/callback?code=...&state=..."}'
```

Use the **actual** returned callback port and redirect. Codes are exchanged once;
failed/expired/consumed attempts need a fresh GET. A successful exchange is saved
independently of the submitting client's disconnect, within a bounded lifetime.

The old account remains active until the candidate's history is prepared and its
credentials are committed. Refresh and replacement are serialized. In-flight requests
retain their original credentials and cannot recover using the new account's tokens.
History is isolated per account and survives restart and switching back. Start a new
conversation after switching: prior encrypted reasoning belongs to the old account.
External account changes in `auth.json` still fail closed; use runtime login or restart.

## Clients

### Official Codex CLI

Use [`examples/codex.config.toml`](examples/codex.config.toml):

```toml
model_provider = "codex_proxy"
model = "gpt-5.5"

[model_providers.codex_proxy]
name = "codex-proxy"
base_url = "http://127.0.0.1:8080/v1"
wire_api = "responses"
requires_openai_auth = false
supports_websockets = false
# env_key = "CODEX_PROXY_API_KEY"
```

HTTP/SSE and uncompressed JSON only. The CLI owns tool execution and the agent loop.

### OpenAI SDK

```python
import os
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:8080/v1",
    api_key=os.environ.get("CODEX_PROXY_API_KEY", "local"),
)
print(client.responses.create(model="gpt-5.5", input="Reply with hello.").output_text)
```

This key authenticates to the proxy, not OpenAI. Installing the SDK is optional.

### Bifrost

Merge [`examples/bifrost.config.json`](examples/bifrost.config.json) into Bifrost's
configuration. It uses an **OpenAI-based custom provider**, verified against
Bifrost **core v1.11.3**, following its [OpenAI provider documentation](https://docs.getbifrost.ai/providers/supported-providers/openai#openai).

- Run the proxy on `127.0.0.1:8081` to avoid Bifrost's default gateway port `8080`.
- Provider base URL: **`http://127.0.0.1:8081`**, without `/v1`; Bifrost appends it.
- Share only `CODEX_PROXY_API_KEY`, never the management key or Codex credentials.
- Enable `disable_store`; allow `responses`, `responses_stream`, and `list_models`.
- Call Bifrost using `codex-proxy/gpt-5.5`, or `your-provider-name/gpt-5.5` if renamed.
  Bifrost removes its routing prefix before calling the proxy. No double prefix.

```sh
curl http://127.0.0.1:8080/openai/v1/responses \
  -H 'Content-Type: application/json' \
  -d '{"model":"codex-proxy/gpt-5.5","input":"Reply with hello.","stream":false}'
```

Add your gateway's authentication header if required; that key is separate too.
For Docker/private networks, use a reachable proxy address and Bifrost's
`network_config.allow_private_network: true`. Loopback belongs to each container.

## HTTP surface

| Route | Authentication | Purpose |
| --- | --- | --- |
| `GET /health` | Public | Liveness and credential file presence; no upstream call |
| `GET /v1/models` | Inference key, when configured | Account's live OpenAI-style model list |
| `GET /v1/models?client_version=0.162.1` | Inference | Native Codex catalog |
| `POST /v1/responses` | Inference | Responses JSON or SSE |
| `POST /v1/responses/compact` | Inference | Upstream-dependent compaction passthrough |
| `GET /status/account` | Management key | Identity hints and available plan metadata |
| `GET /status/limit` | Management key | Latest cached quota observation and staleness |
| `GET /status/usage` | Management key | Filtered, paginated persistent observations |
| `GET /config/login` | Management key | Begin/reuse a pending browser login |
| `POST /config/login` | Management key | Complete login and replace the active account |

Inference aliases without `/v1` and under `/backend-api/codex` remain available.
Management routes have no inference aliases. Authentication requires headers (no
query parameters).

**Compatibility notes:** Codex requires `stream:true` / `store:false` upstream.
Both streaming and non-streaming modes restore omitted terminal output. Tool calls,
reasoning, and usage retain native shapes. Full input history is required;
`previous_response_id`, background execution, and stored prompt references are
not supported by the Codex backend. Sampling and output-limit parameters unsupported
by Codex are ignored. See [API details](docs/api.md).

## Safety and configuration

- Loopback by default; non-loopback requires `CODEX_PROXY_API_KEY`. Remote access
  additionally needs TLS and a firewall or encrypted tunnel.
- Restrict management key access to administrators; it permits account replacement.
- Upstream and quota URLs are operator-controlled trust boundaries receiving access
  tokens. HTTPS only (except local loopback tests); redirects are never followed.
- The proxy locks its auth file and history against other proxy processes. **Official
  Codex does not honor those locks.** Do not run a directly authenticated Codex process
  or another token-refreshing tool against the same auth file concurrently.
- Request limits: bodies up to 16 MiB, SSE frames up to 16 MiB, buffered output up to 32 MiB.
- No prompts, responses, token values, OAuth codes, or authorization URLs are logged.
  Credentials, histories, and runtime lock files must remain untracked.

For more details:
- [Configuration](docs/configuration.md) — full flag and environment reference
- [Deployment](docs/deployment.md) — remote setup and process management, including
  a [hardened systemd service](docs/deployment.md#systemd-linux) for native Linux installs
- [Architecture](docs/architecture.md) — internal design and lifecycles
- [API Reference](docs/api.md) — endpoints and payloads

## Testing

Run the test suite:

```sh
./scripts/test.sh
# Or:
go test -race ./...
```

Strict linting and formatting use pinned development tooling (not runtime dependencies):

```sh
./scripts/lint.sh
# Optional fake-backend Bifrost compatibility test (never uses real credentials):
CODEX_PROXY_BIFROST_TEST=1 go test -race -run '^TestBifrostCompatibility$' ./internal/proxy
```

CI tests the minimum Go 1.22, the Docker builder's exact Go 1.27.1, and stable Go.
The lint script uses v2.14.0 from `PATH` when available; otherwise it automatically
installs that pinned version under `$XDG_CACHE_HOME/codex-proxy/tools` (or
`$HOME/.cache/codex-proxy/tools`). It does not overwrite an older global linter.
Set `GOLANGCI_LINT=/absolute/path/to/golangci-lint` to use an explicit v2.14.0 binary;
an incorrect version fails with a clear diagnostic. First installation needs network
access. The tool needs Go 1.26+; automatic toolchain selection can install it without
raising the production module's Go 1.22 minimum. Live-account tests remain explicit opt-ins.
