# Deployment

Build the standard-library-only binary:

```sh
go build -trimpath -ldflags='-s -w' -o bin/codex-proxy ./cmd/codex-proxy
```

Run under a dedicated non-root user. Its credential directory must be writable for
OAuth refresh, runtime login and the process lock. Give its private history directory
mode `0700` and keep keys outside the checkout. Start with environment variables:

```sh
CODEX_PROXY_API_KEY="your-inference-key" \
CODEX_PROXY_MANAGEMENT_KEY="a-different-administrator-key" \
CODEX_PROXY_ADDR="127.0.0.1:8081" \
./bin/codex-proxy serve
```

The strings above are placeholders, not credentials. A supervisor such as systemd can
supply a private environment file; never commit that file. SIGINT/SIGTERM drain HTTP
requests for ten seconds before closing remaining connections. Refresh completion is
joined so rotated tokens are not abandoned. Login/polling workers and locks are closed.

## Remote and container login

Bind the inference/management HTTP server to a reachable interface only behind TLS,
a firewall or an encrypted tunnel. A non-loopback bind requires an inference key.
Management additionally requires its distinct key even on loopback. Give Bifrost only
the inference key; restrict `/status/*` and `/config/*` to administrator networks.

The browser OAuth callback stays **loopback-only**, on port `1455` or `1457`. A browser
on another machine cannot reach that loopback. Either:

1. Use an SSH tunnel forwarding the actual callback port to the remote process's
   loopback, or
2. Copy the final redirect URL from the browser and POST it to `/config/login` over
   the protected management channel. This works even if the browser displays a
   connection-refused page after the provider redirects.

In containers, loopback belongs to the container. Publishing a port does not expose
a listener bound only to container loopback. Prefer manual redirect completion, and
provide writable private volumes for both Codex credentials and quota history. Do not
mount the credential file read-only if refresh/login is expected to work. No Docker
image/release automation is added by this refactor; use the compiled binary in your
existing deployment tooling.

## Credential ownership and switching

Run only one refreshing process per credential file. Another proxy is excluded by
our lock, but official Codex does not participate. A directly authenticated Codex
process can still race refresh-token rotation. Codex configured to use this proxy's
custom provider does not need to own upstream authentication.

Browser login deliberately replaces the configured native auth file. Use a separate
`-auth-file` path if you do not want management login to replace your existing Codex
CLI login. Refresh preserves unknown metadata; explicit replacement writes a fresh
account document so old-account metadata is not carried into a different login.

Hot-switching is not account pooling or automatic failover. New requests use the new
account; old requests are not replayed under it. Start a new client conversation after
switching because encrypted reasoning from the old account may not be accepted.

## History operations

Histories contain account quota measurements, not prompts, responses or credentials.
They are still private operational data. Do not commit them or upload them to public
logs. Retention compacts journals to bounded private files; only incomplete trailing
records are recovered automatically. Complete corruption and account misattribution
fail closed. Inspect/back up storage while the proxy is stopped before remediation.

Do not remove live `.lock` files. OS locks release after a crash, so restart does not
require deleting stale filenames. Persistent history can remain in place across normal
upgrades and switches back to an account.
