# Configuration

Flags override environment defaults, including invalid quota interval defaults.
`serve` is optional; bare flags still work. Explicit auth/history paths do not require
a resolvable home directory; missing defaults fail rather than becoming relative paths.

| Flag | Environment | Default |
| --- | --- | --- |
| `-addr` | `CODEX_PROXY_ADDR` / `HOST` + `PORT` | `127.0.0.1:8080` |
| `-config` | `CODEX_PROXY_CONFIG_FILE` | `$XDG_CONFIG_HOME/codex-proxy/config.json` or `~/.config/codex-proxy/config.json` |
| `-auth-file` | `CODEX_PROXY_AUTH_FILE` | `$CODEX_HOME/auth.json`, otherwise `~/.codex/auth.json` (legacy fallback) |
| — | `CODEX_PROXY_API_KEY` / `API_KEY` | Empty; default account inference key |
| — | `ADMIN_API_KEY` / `CODEX_PROXY_MANAGEMENT_KEY` | Empty; admin management routes |
| `-upstream` | `CODEX_PROXY_UPSTREAM` | `https://chatgpt.com/backend-api/codex` |
| `-usage-url` | `CODEX_PROXY_USAGE_URL` | Derived from upstream; normally `https://chatgpt.com/backend-api/wham/usage` |
| `-client-version` | `CODEX_PROXY_CLIENT_VERSION` | `0.162.1` |
| — | `CODEX_PROXY_USER_AGENT` | `codex_cli_rs/<version> (<OS>; <architecture>)` |
| `-idle-timeout` | — | `5m` |
| `-quota-poll-interval` | `CODEX_PROXY_QUOTA_POLL_INTERVAL` | `5m`; valid `10s`–`24h` |
| `-history-dir` | `CODEX_PROXY_HISTORY_DIR` | `$XDG_DATA_HOME/codex-proxy` or `~/.local/share/codex-proxy` |
| `-history-max-samples` | — | `10000`; valid `1`–`10000` |

## Configuration File

The default configuration file is located at:
```text
~/.config/codex-proxy/config.json
```

On Unix systems, this file must have owner-only permissions (`0600`).

### Schema & Multi-Account Structure

```json
{
  "addr": "127.0.0.1:8080",
  "adminKeyHash": "sha256:d8a2...3f1c",
  "accounts": [
    {
      "id": "work",
      "label": "Work Account",
      "enabled": true,
      "refreshToken": "...",
      "accessToken": "...",
      "idToken": "...",
      "accountId": "chatgpt-account-id",
      "accountEmail": "developer@company.com",
      "accountName": "Jane Developer",
      "planType": "plus",
      "keys": [
        {
          "id": "key_3f9a1c2e",
          "label": "bifrost",
          "hash": "sha256:4b8e...",
          "last4": "2m3f",
          "createdAt": "2026-10-16T12:00:00Z"
        }
      ]
    }
  ],
  "upstream": "https://chatgpt.com/backend-api/codex",
  "clientVersion": "0.162.1",
  "pollInterval": "5m",
  "historyDir": "/home/user/.local/share/codex-proxy"
}
```

### Migration from Single-Account Configurations

Codex Proxy automatically migrates existing single-account credentials on startup:
- If a legacy `auth.json` file is present (or flat legacy fields exist in `config.json`), it is converted in-memory into account `default`.
- Any legacy `API_KEY` becomes `key_legacy` attached to `default`.
- The legacy `auth.json` file is left completely untouched.
- Subsequent administrative mutations (e.g. creating keys, adding accounts via login sessions or CLI) write the multi-account structure to `config.json`.

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
16 MiB per-account file size bound retention. API-visible retention stays at the
configured count; physical journals allow 25% superseded-record slack (at least one)
between compactions. At byte pressure, compaction targets 80% of the cap; it may drop
older observations to leave room for appends. At most two journals are cached in
memory; switching back reopens older retained journals from disk. An unavailable
active identity prevents third-journal eviction until identity can be established.

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
