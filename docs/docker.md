# Running NEXUS in Docker

NEXUS's primary install path is the single binary (`curl | sh` — see the
[README](../README.md)). This page is for the optional alternative: running
NEXUS as a container, e.g. so it comes back automatically after a host
reboot.

## Quick start

```bash
git clone https://github.com/lynuxis2026-pixel/nexus-proxy.git
cd nexus-proxy
docker compose up -d --build
```

This builds the image locally (Node stage builds the real dashboard, Go
stage compiles the binary — see [`Dockerfile`](../Dockerfile)) and starts
NEXUS with:

- proxy on `http://localhost:3000`
- dashboard on `http://localhost:2222`
- an anonymous-turned-persistent volume `nexus-data` mounted at
  `/home/nexus/.nexus` inside the container (config + SQLite DB — the same
  `~/.nexus/` the binary uses outside Docker)

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
env auto-discovery and need an explicit `nexus add`. Run it as a one-off
against the persistent volume, then restart the main service:

```bash
docker run --rm -v nexus-data:/home/nexus/.nexus \
  $(docker compose config --images | head -1) \
  add openrouter sk-xxx --type openai-compatible --base-url https://openrouter.ai/api/v1

docker compose restart nexus
```

## Diagnostics without a shell

The final image is `distroless` — no shell, no coreutils, smaller attack
surface. Use the binary's own diagnostics instead of exec-ing into a shell:

```bash
docker exec nexus /nexus status   # provider health
docker exec nexus /nexus doctor   # full diagnostic
docker logs -f nexus              # live logs
```

## Persistence

Data lives in the `nexus-data` named volume, mounted at
`/home/nexus/.nexus` (config.toml + nexus.db). It survives
`docker compose down` / `up`, image rebuilds, and container recreation.
`docker compose down -v` deletes it — avoid that unless you mean to reset.

## TLS / reverse proxy

If you front NEXUS with Caddy, nginx, or similar for TLS on a home server,
disable response buffering on the dashboard's SSE stream
(`GET /events`) — buffering breaks live updates. For nginx:
`proxy_buffering off;` on that location block.
