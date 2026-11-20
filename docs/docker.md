# Running NEXUS in Docker

NEXUS's primary install path is the single binary (`curl | sh` — see the
[README](../README.md)). This page is for the optional alternative: running
NEXUS as a container, e.g. so it comes back automatically after a host
reboot.

## Quick start

```bash
git clone https://github.com/lynuxis2026-pixel/nexus-proxy.git
cd nexus-proxy
mkdir -p data                                     # see "Editing config.toml" below for why
docker compose up -d --build
```

This builds the image locally (Node stage builds the real dashboard, Go
stage compiles the binary — see [`Dockerfile`](../Dockerfile)) and starts
NEXUS with:

- proxy on `http://localhost:3000`
- dashboard on `http://localhost:2222`
- `./data` bind-mounted to `/home/nexus/.nexus` inside the container
  (config.toml + nexus.db — the same `~/.nexus/` the binary uses outside
  Docker), so it's a plain directory on the host, not hidden inside Docker's
  internal volume storage

Once a version is tagged, a prebuilt multi-arch (amd64/arm64) image is also
published to `ghcr.io/lynuxis2026-pixel/nexus-proxy`. Swap the `build: .`
line in `docker-compose.yml` for `image: ghcr.io/lynuxis2026-pixel/nexus-proxy:latest`
to pull instead of building — worth doing on lower-powered boards
(Raspberry Pi / Orange Pi) where building the Node+Go stages locally is slow.

## Surviving a reboot

`docker-compose.yml` sets `restart: unless-stopped`. Combined with the
Docker daemon itself starting on boot (the default once Docker is
installed via systemd on most Linux distros), the container comes back on
its own after the host restarts — no crontab or systemd unit needed for
NEXUS itself. Verify the daemon is enabled with:

```bash
systemctl is-enabled docker
```

## Editing config.toml directly from the host

`./data` is a plain bind-mounted directory, not a Docker-managed volume, so
`./data/config.toml` and `./data/nexus.db` are just files on the host — open
`config.toml` in any editor, no `docker exec` needed. Field reference: the
[Add providers](../README.md#add-providers) section of the README, or the
`Provider` struct in
[`internal/config/config.go`](../internal/config/config.go) for every
optional field (`model_map`, off-peak pricing, `api_keys` pools, etc).

**Create `./data` yourself before the first `docker compose up`.** If it
doesn't exist yet, Docker auto-creates it owned by `root`, which the
container's nonroot process then can't write to — `nexus start` will fail
with a permission error and (since the service restarts) crash-loop until
`./data` has the right owner.

To edit as your own host user (no `sudo` needed to open the file):

```bash
mkdir -p data
printf 'NEXUS_UID=%s\nNEXUS_GID=%s\n' "$(id -u)" "$(id -g)" >> .env
docker compose up -d --build
```

`NEXUS_UID`/`NEXUS_GID` (read from `.env`) tell the container to run as your
uid:gid instead of the image's default nonroot user (65532), so `./data`'s
ownership matches your login on both sides. If you skip this, `mkdir -p
data` still works, but only `sudo` (or `sudo chown 65532:65532 data`) can
edit the files afterwards.

NEXUS reads `config.toml` once at process startup, same as the plain
binary — after editing it, apply changes with:

```bash
docker compose restart nexus
```

## Configuring providers

### Env vars (recommended, no file needed)

NEXUS auto-discovers ~28 providers from environment variables on every
request (see `internal/config/config.go`'s `knownEnvKeys` — e.g.
`GROQ_API_KEY`, `DEEPSEEK_API_KEY`, `GEMINI_API_KEY`, `ANTHROPIC_API_KEY`,
`OPENROUTER_API_KEY`, ...). Put them in a `.env` file next to
`docker-compose.yml` (already referenced via `env_file:`):

```bash
# .env
GROQ_API_KEY=gsk-xxx
DEEPSEEK_API_KEY=sk-xxx
```

No config file, no `nexus add`, no rebuild — just restart the container.

### Custom / enterprise endpoints

`--type openai-compatible|azure|vertex|bedrock` endpoints aren't covered by
env auto-discovery. Since `./data/config.toml` is host-editable (see above),
either add a `[[providers]]` block to it by hand and `docker compose restart
nexus`, or run `nexus add` as a one-off against the same bind mount — it
validates the config and fills in tier/models for you:

```bash
docker run --rm -v "$(pwd)/data:/home/nexus/.nexus" \
  --user "${NEXUS_UID:-65532}:${NEXUS_GID:-65532}" \
  $(docker compose config --images | head -1) \
  add openrouter sk-xxx --type openai-compatible --base-url https://openrouter.ai/api/v1

docker compose restart nexus
```

### Qwen Code subscription

For dashboard sign-in, set `NEXUS_DASHBOARD_ADMIN_TOKEN` in the Compose `.env`
file to a private operator secret. Recreate the service with `docker compose up -d`
to apply the environment change. Open the dashboard and choose **Connect Qwen**.
Enter that operator token, start Qwen sign-in, and open **Approve on Qwen** in
your browser. Return to the dashboard to see the result. Use the dashboard over
HTTPS or through an SSH tunnel when connecting from another machine.

To sign in from a terminal, use the same NEXUS image and data mount as the service.
Open the URL printed by the command and approve it in your browser.

```bash
docker compose run --rm nexus auth qwen-subscription
```

Both sign-in paths save the session and add `qwen-subscription` to `config.toml`
if needed. To declare the provider yourself, use this entry and sign in through
either path.

```toml
[[providers]]
name = "qwen-subscription"
type = "qwen-subscription"
```

After adding the provider for the first time, run `docker compose restart nexus`.
The dashboard shows whether models are ready. With catalog discovery enabled,
clients can list the subscription's model IDs at `/v1/models` and request those
IDs through the proxy. A running proxy with Qwen loaded adopts a successful
re-login automatically.

Keep the `qwen/` subdirectory of the NEXUS data mount private. It contains the
subscription's active credentials. NEXUS refreshes and saves those credentials
automatically. If the session needs a new sign-in, use **Connect Qwen** or the
same terminal command again.

## Diagnostics without a shell

The final image is `distroless` — no shell, no coreutils, smaller attack
surface. Use the binary's own diagnostics instead of exec-ing into a shell:

```bash
docker exec nexus /nexus status   # provider health
docker exec nexus /nexus doctor   # full diagnostic
docker logs -f nexus              # live logs
```

## Persistence

Data lives in `./data` on the host (config.toml + nexus.db), bind-mounted to
`/home/nexus/.nexus`. Being a plain directory rather than a Docker-managed
volume, it survives `docker compose down` / `up`, image rebuilds, container
recreation, and `docker compose down -v`. Keep backups of the data directory
private because they can contain API keys and Qwen session credentials. For
config history, track a copy of `config.toml` with any secrets removed.

Prefer a Docker-managed named volume instead (e.g. for portability across
hosts, or to avoid the UID/GID setup above)? Swap the `volumes:` entry in
`docker-compose.yml` for a named volume (`nexus-data:/home/nexus/.nexus` +
a top-level `volumes: { nexus-data: }` block) and drop the `user:` line —
the image's `Dockerfile` still pre-seeds ownership for that path so the
default nonroot user can write to a fresh named volume. You lose direct
host-file editing that way — back to `docker exec`/`docker cp`.

## TLS / reverse proxy

If you front NEXUS with Caddy, nginx, or similar for TLS on a home server,
disable response buffering on the dashboard's SSE stream
(`GET /events`) — buffering breaks live updates. For nginx:
`proxy_buffering off;` on that location block.

For dashboard Qwen sign-in behind a TLS proxy, preserve the browser-facing
`Host` and set `X-Forwarded-Proto: https` on requests forwarded to NEXUS.
