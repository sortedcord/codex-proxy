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

## Docker

Build the local image:

```sh
docker build -t codex-proxy .
```

Like Antigravity Proxy, this is a multi-stage build with a static Go binary, CA
certificates and a `scratch` runtime. It has no shell/package manager and runs as
UID/GID **10001:10001**. The build context allowlist excludes credentials, histories,
local binaries, reference checkouts, and the test-only Bifrost module.

The image defaults to `0.0.0.0:8080`, **requiring `CODEX_PROXY_API_KEY`**. Supply keys
at runtime, never as build arguments. Export the inference key and, if enabling
management/login/history, a different `CODEX_PROXY_MANAGEMENT_KEY` before running:

```sh
docker volume create codex-proxy-auth
docker volume create codex-proxy-history

docker run -d \
  --name codex-proxy \
  --restart unless-stopped \
  --read-only \
  --cap-drop ALL \
  --security-opt no-new-privileges:true \
  --mount type=volume,src=codex-proxy-auth,dst=/home/app/.codex \
  --mount type=volume,src=codex-proxy-history,dst=/home/app/.local/share/codex-proxy \
  -p 127.0.0.1:8081:8080 \
  -e CODEX_PROXY_API_KEY \
  -e CODEX_PROXY_MANAGEMENT_KEY \
  codex-proxy

curl http://127.0.0.1:8081/health
```

Fresh volumes inherit the image's private `0700` directory permissions and ownership.
They survive container replacement. Existing volumes must be writable by the container
user and remain private. Keep **both volumes writable**, even with a read-only root:
refresh/login saves atomically replace `auth.json`, and the auth process lock lives
beside it. Mount the credential **directory**, not a single `auth.json` file; a file
bind mount prevents atomic replacement. Do not delete volumes or live lock files.

A fresh credential volume starts without an account. Complete runtime login using the
management APIs; inference returns `503` until configured. Keep the default loopback
OAuth callback private and use **manual redirect completion** from an outside browser,
as described below and in [the API reference](api.md#login-and-account-replacement).
No OAuth callback ports need publishing. Omit the management environment variable
if you want inference-only operation with an already configured credential directory.

### Reuse host Codex credentials

Alternatively, use private writable bind mounts and run as their **non-root owner**.
For a non-root host user with existing `codex login` credentials:

```sh
mkdir -p "$HOME/.local/share/codex-proxy"
chmod 700 "$HOME/.local/share/codex-proxy"

docker run -d \
  --name codex-proxy \
  --restart unless-stopped \
  --read-only \
  --cap-drop ALL \
  --security-opt no-new-privileges:true \
  --user "$(id -u):$(id -g)" \
  --mount "type=bind,src=${CODEX_HOME:-$HOME/.codex},dst=/home/app/.codex" \
  --mount "type=bind,src=$HOME/.local/share/codex-proxy,dst=/home/app/.local/share/codex-proxy" \
  -p 127.0.0.1:8081:8080 \
  -e CODEX_PROXY_API_KEY \
  -e CODEX_PROXY_MANAGEMENT_KEY \
  codex-proxy
```

Stop/remove the named-volume example container before using the same name/port here.
Do not run a directly authenticated Codex client against those credentials while the
proxy owns refresh. Runtime login deliberately replaces this host's Codex auth file;
choose a dedicated writable auth directory if that is not desired.

### Bifrost on a Docker network

On a shared private Docker network, set Bifrost's custom OpenAI provider base URL to
**`http://codex-proxy:8080`**, without `/v1`, and enable
`network_config.allow_private_network: true`. Give Bifrost only the inference key.
Host port `8081` is for host clients; another container cannot use the host's loopback
address. Restrict administrator access with network/firewall rules or an encrypted
tunnel; mapping the host port to loopback does not restrict peers on the Docker network.
No published registry image or image-release automation is assumed; build locally.

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
mount the credential directory read-only: refresh, runtime login and the process lock
all require writes. The Docker setup above supports a read-only root filesystem with
writable persistent auth/history mounts.

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
