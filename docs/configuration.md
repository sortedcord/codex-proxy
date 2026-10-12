# Configuration

Flags override environment defaults. `serve` is optional; bare flags still work.

| Flag | Environment | Default |
| --- | --- | --- |
| `-addr` | `CODEX_PROXY_ADDR` | `127.0.0.1:8080` |
| `-auth-file` | `CODEX_PROXY_AUTH_FILE` | `$CODEX_HOME/auth.json`, otherwise `~/.codex/auth.json` |
| — | `CODEX_PROXY_API_KEY` | Empty; inference is unprotected on loopback |
| — | `CODEX_PROXY_MANAGEMENT_KEY` | Empty; management/polling/history disabled |
| `-upstream` | `CODEX_PROXY_UPSTREAM` | `https://chatgpt.com/backend-api/codex` |
| `-usage-url` | `CODEX_PROXY_USAGE_URL` | Derived from upstream; normally `https://chatgpt.com/backend-api/wham/usage` |
| `-client-version` | `CODEX_PROXY_CLIENT_VERSION` | `0.162.1` |
| — | `CODEX_PROXY_USER_AGENT` | `codex_cli_rs/<version> (<OS>; <architecture>)` |
| `-idle-timeout` | — | `5m` |
| `-quota-poll-interval` | `CODEX_PROXY_QUOTA_POLL_INTERVAL` | `5m`; valid `10s`–`24h` |
| `-history-dir` | `CODEX_PROXY_HISTORY_DIR` | `$XDG_DATA_HOME/codex-proxy` or `~/.local/share/codex-proxy` |
| `-history-max-samples` | — | `10000`; valid `1`–`10000` |

The proxy has no additional JSON credential import or configuration database.
Credentials remain in Codex's native auth file. Secrets are environment-only, not
CLI flags, to avoid appearing in process arguments. Inference and management keys
must differ; only management possession permits account status/history and login.

The upstream and quota URLs receive your account token. Supply only trusted HTTPS
URLs (HTTP permitted only on loopback), without user info, query or fragment. When
using a custom backend not ending in `/codex`, the derived quota URL is its base plus
`/usage`; set `-usage-url` explicitly if that backend exposes a different route.
Neither bearer credentials nor authorization codes follow redirects. Go's ordinary
`HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY` settings apply to outbound transport.

Management-disabled operation does not create/open quota history or poll quotas.
When enabled, history is scoped to workspace plus provider-reported user identity
(legacy credentials without user identity fall back to account ID), in hashed
`usage-<sha256>.jsonl` files, mode
`0600`, in a private `0700` directory. Existing overly permissive directories are
rejected rather than silently changing permissions. Both retained sample count and
16 MiB per-account file size bound retention. At most two journals are cached in
memory; switching back reopens older retained journals from disk.

Separate lock files are private and intentionally retained after exit. OS locks
release on process termination; do not unlink a live lock file. The proxy also locks
`<auth-file>.codex-proxy.lock`, excluding another proxy that uses the same auth file.
These locks do not coordinate with the official Codex client or other software.

POSIX saves sync file content, rename atomically, then sync the containing directory.
Windows uses file sync and rename; Go cannot provide the same directory-sync guarantee.
Protect Windows credential/history directories with appropriate ACLs. Linux, macOS,
BSD and Windows have native process locks; unsupported platforms fail closed.

Malformed or unsupported existing auth documents fail startup. An absent file is
allowed so an administrator can complete runtime login. External same-account token
rotation reloads on requests; logout/unrecognized account replacement fails closed.
Only explicit successful management login publishes an account hot-switch.
