# Deployment

Build the standard-library-only binary:

```sh
./scripts/build.sh
./bin/codex-proxy version
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

## systemd (Linux)

The packaged [`codex-proxy.service`](../deploy/systemd/codex-proxy.service) targets
**systemd 249+** (Ubuntu 22.04 or newer). CI exercises Ubuntu 22.04/24.04; the unit
also has a local runtime smoke test on systemd 262. Other distributions must provide
systemd's usual namespace/sandbox support; restricted containers may not support it.
It is a system service, not a `systemctl --user` unit.

### Install and configure

Build or obtain a trusted native binary, then install the binary and unit as root:

```sh
./scripts/build.sh
sudo install -o root -g root -m 0755 bin/codex-proxy /usr/local/bin/codex-proxy
sudo install -o root -g root -m 0644 deploy/systemd/codex-proxy.service \
  /etc/systemd/system/codex-proxy.service
sudo install -d -o root -g root -m 0700 /etc/codex-proxy
# First install only: do not overwrite an existing configuration during upgrades.
sudo install -o root -g root -m 0600 deploy/systemd/codex-proxy.env.example \
  /etc/codex-proxy/codex-proxy.env
sudoedit /etc/codex-proxy/codex-proxy.env
```

The configuration uses **systemd EnvironmentFile syntax**, not shell syntax: no
`export`, command substitution, or inline comments. Generate private random keys
(for example, with `openssl rand -hex 32`) outside this repository and place them in
the installed file, never in the unit, command arguments, or committed template.
Keys in the process environment are accessible to trusted root administrators; this
is not a secret store that protects against root. Real `*.env` files are ignored.

The template binds to **127.0.0.1:8081**. Its empty inference key is valid for
loopback-only use; set a key to protect inference even locally. An empty management
key disables management, login, polling, and history. Enable browser login/status
by supplying a **different** management key. Changing to a non-loopback address
requires an inference key **and** TLS plus a firewall or encrypted tunnel. Keep
management endpoints restricted to administrators. Restart after editing the file.

```sh
sudo systemd-analyze verify /etc/systemd/system/codex-proxy.service
sudo systemctl daemon-reload
sudo systemctl enable --now codex-proxy.service
sudo systemctl status codex-proxy.service
curl --fail http://127.0.0.1:8081/health
sudo journalctl -u codex-proxy.service -n 50 --no-pager
/usr/local/bin/codex-proxy version
```

### Account and persistent state

`DynamicUser=yes` lets systemd allocate a dedicated **non-root, non-login** account;
there is no `useradd` step or dependency on an operator's home. `StateDirectory`
creates and manages `/var/lib/codex-proxy` with mode **0700**. Under DynamicUser,
systemd stores it below `/var/lib/private` and exposes it through a managed symlink;
do not replace that link, hard-code a numeric UID, or manually chown it to a
previously allocated UID. Ownership/id mapping is managed across restarts.

The unit fixes these paths with command-line flags:

- Credentials: `/var/lib/codex-proxy/auth/auth.json`.
- History: `/var/lib/codex-proxy/history`.
- Auth/history process locks live beside their state and remain after clean exit.

The root-owned executable/configuration and system filesystem are not writable by
the service. Only its persistent state and private temporary storage are writable.
Credential directories remain writable for atomic replacement and directory sync;
never bind-mount only `auth.json` or make the credential directory read-only.
Changing these paths requires a reviewed unit override, not environment values
(the command-line flags take precedence).

A clean installation starts **without credentials**. Health succeeds; inference
returns `503` for authenticated requests until an account is configured. With the
management key enabled, begin runtime browser login using the existing
[management instructions](../README.md#runtime-browser-login-and-hot-switching).
For a remote host, use [manual redirect completion or an SSH callback tunnel](#remote-and-container-login).
Do not expose callback ports 1455/1457 publicly. Login writes this dedicated service
state, not the operator's `~/.codex/auth.json`. Do not run another token-refreshing
Codex process against the service credential file.

### Sandboxing and shutdown

The unit uses `ProtectSystem=strict`, `ProtectHome`, private temporary/device
namespaces, `NoNewPrivileges`, empty capability sets, kernel/control-group
protection, and restrictions on setuid and personality changes. It allows UNIX,
IPv4, and IPv6 sockets for DNS, outbound HTTPS, inference, and loopback callbacks;
it deliberately does **not** use `PrivateNetwork` or an unvalidated syscall filter.
StateDirectory is automatically writable despite filesystem protection. If sandbox
setup fails on a restricted host, fix the host support or deliberately review an
override; do not silently disable the hardening.

Failures restart after five seconds, limited to five starts per minute so invalid
configuration does not loop indefinitely. After fixing a failed configuration:

```sh
sudo systemctl reset-failed codex-proxy.service
sudo systemctl start codex-proxy.service
```

SIGTERM drains HTTP requests for up to ten seconds and joins bounded OAuth
persistence workers. `TimeoutStopSec=90s` allows that work to complete before
systemd's forced termination. A normal `systemctl stop` does not trigger restart.

### Upgrade, backup, and rollback

Stop the service before backups, credential import, or recovery. Backups contain
sensitive credentials/private operational state: keep them root-only, outside the
checkout, and never upload them to CI or issue attachments. Do not remove live lock
files. A stopped service does not need lock-file deletion before restart.

Example on a trusted host (choose a new private backup filename each time):

```sh
sudo systemctl stop codex-proxy.service
sudo install -d -o root -g root -m 0700 /var/backups/codex-proxy
# -C through the state symlink backs up the actual contents, not just the link.
sudo sh -c 'umask 077; tar -C /var/lib/codex-proxy -czf /var/backups/codex-proxy/state-before-upgrade.tar.gz .'
sudo install -o root -g root -m 0755 /usr/local/bin/codex-proxy \
  /var/backups/codex-proxy/codex-proxy.previous
sudo install -o root -g root -m 0755 bin/codex-proxy /usr/local/bin/codex-proxy
/usr/local/bin/codex-proxy version
# Install updated unit if needed, but retain the private environment file.
sudo systemctl daemon-reload
sudo systemctl start codex-proxy.service
curl --fail http://127.0.0.1:8081/health
sudo journalctl -u codex-proxy.service -n 50 --no-pager
```

To roll back the executable, stop the service, reinstall `codex-proxy.previous` as
`/usr/local/bin/codex-proxy`, then start and check it. Review release notes for state
format compatibility first. **Do not routinely restore old auth files**: refresh
tokens may have rotated since the backup and old tokens can be invalid. Keep the
current auth state when rolling back a compatible executable; use fresh login if
credentials need recovery. DEB/RPM package installation is not included here.

### Test the packaged unit

On a disposable systemd host with Python 3 and root access:

```sh
./scripts/build.sh
sudo python3 scripts/test-systemd.py ./bin/codex-proxy
```

This opt-in test verifies a temporary copy of the shipped unit, changing only its
account, executable/configuration/state paths and adding an in-sandbox probe. It
uses dummy keys and no live credentials, checks non-root execution, private atomic
state writes, filesystem/capability restrictions, loopback OAuth callback binding,
authentication, graceful stop, released locks, and persistent state across starts.
It removes only its own temporary unit/state after stopping. It neither installs
nor enables the production service. CI runs it on Ubuntu 22.04 and 24.04.

## Docker

Pre-built multi-arch images (`linux/amd64`, `linux/arm64`) are published to GitHub
Container Registry at `ghcr.io/sortedcord/codex-proxy`.

For production deployments, pin an exact release version or image digest:

```sh
# Pinned stable release:
docker pull ghcr.io/sortedcord/codex-proxy:0.5.0

# Or immutable digest:
docker pull ghcr.io/sortedcord/codex-proxy@sha256:...
```

### Container release channels

- **`0.5.0` (or `v0.5.0`)**: Exact, immutable release build.
- **`0.5`**: Moving minor alias for the latest patch release in the 0.5 series.
- **`latest`**: Tracks the most recent stable release. Never updated by pushes to `main`
  or prerelease tags.
- **`main`**: Bleeding-edge development image published on pushes to `main`.
- **`sha-<commit>`**: Commit-addressed immutable image for any pushed commit.

Older maintenance releases will not regress `latest` if a newer stable release exists.

### Docker Compose

A ready-to-run, hardened Compose configuration is provided in [`deploy/compose.yaml`](../deploy/compose.yaml):

```sh
# 1. Copy the environment template
cp deploy/.env.example deploy/.env
chmod 600 deploy/.env

# 2. Configure keys (CODEX_PROXY_API_KEY is required)
# Generate secrets outside the repository, e.g. with `openssl rand -hex 32`
editor deploy/.env

# 3. Start the proxy
docker compose -f deploy/compose.yaml --env-file deploy/.env up -d

# 4. Check health
curl http://127.0.0.1:8081/health
```

The Compose definition encodes all security defaults:
- **Non-root user**: runs as container UID/GID `10001:10001`.
- **Read-only root filesystem**: `read_only: true`.
- **Minimal privileges**: `cap_drop: [ALL]` and `no-new-privileges:true`.
- **Loopback binding**: published on `127.0.0.1:8081:8080` by default.
- **Persistent writable state**: separate named volumes for `/home/app/.codex` and
  `/home/app/.local/share/codex-proxy`. Both volumes must remain writable for atomic
  token rotation, the auth lock, and quota history.
- **Required key enforcement**: Compose interpolation requires `CODEX_PROXY_API_KEY`.
- **Graceful shutdown**: 30-second stop grace period ensures in-flight requests and
  token refresh persistence complete before container termination.

#### Upgrades and maintenance

- To upgrade: `docker compose -f deploy/compose.yaml --env-file deploy/.env pull && docker compose -f deploy/compose.yaml --env-file deploy/.env up -d`.
- **Never run `docker compose down -v` during upgrades**: the `-v` flag removes
  persistent auth and quota volumes.
- To back up state, stop the container first:
  ```sh
  docker compose -f deploy/compose.yaml --env-file deploy/.env stop
  # Inspect or back up volume data while stopped
  docker compose -f deploy/compose.yaml --env-file deploy/.env start
  ```
- No OAuth callback ports are published to the host. Complete runtime login via the
  management API using external manual redirect completion.

To build the image locally:

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
Images are published to GHCR, or you can build locally using the multi-stage Dockerfile.

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
