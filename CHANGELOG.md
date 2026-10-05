## Unreleased

- **Fixed:** usage drill-down error lifecycle — a failed events fetch now
  shows an error (and clears on recovery) instead of a silently empty table.

## v0.8.0

- **Image support on the OpenAI gateway (`/v1/chat/completions`):**
  requests with `image_url` content parts (an `http(s)` URL or a base64
  `data:` URI) are now accepted instead of failing with a 400
  (`unsupported content part type "image_url"`). This affects OpenAI-style
  clients such as pi.dev, opencode and Cursor. OpenAI-compatible providers
  receive the image as sent, including the `detail` field; when the request
  is routed to an Anthropic, Bedrock or Vertex provider, the image is
  converted to an Anthropic image block. Malformed `image_url`
  parts and other unsupported part types still return a clear 400. NEXUS
  does not fetch image URLs: they are forwarded as received, and URL support
  and accepted formats depend on the provider (its own error is relayed
  unchanged).
- **Image fixes in the firewall, cache, vision override and inspector:**
  the privacy firewall no longer scans or rewrites base64 image payloads
  (a secret-shaped substring inside the base64 could previously corrupt the
  image); the semantic cache now also skips OpenAI-shaped `image_url`
  requests; image detection recurses into `tool_result` content, so the
  `vision_model` override now applies to images Claude Code reads from disk
  and to requests on the OpenAI gateway, and the logged `model_used`
  (in both the request log and the usage events) names the model
  actually sent (the override applies to OpenAI-compatible
  providers only; the `direct` strategy still forwards the requested model
  verbatim); `--inspect` capture omits long base64 image data instead of
  storing a truncated blob.
- **`direct` routing strategy:** forwards the client's requested model id
  to OpenAI-compatible providers exactly as received, bypassing
  `model_map` and the `vision_model` override — for pointing NEXUS
  straight at a specific provider model (e.g. `glm-5.2`, `zhai/glm-5.2`)
  with zero translation. Provider selection and sticky-until-429 behavior
  stay identical to `auto`. Enable it with `strategy = "direct"` under
  `[routing]` in `config.toml`.
- **Usage measurement (immutable per-attempt events):** every roundtrip
  that leaves the machine — chain failover steps, 429 key rotations,
  cascade candidates, cooldown-recovery probes, and streams that aborted
  mid-flight — now appends one row to a new append-only `usage_events`
  table, which is never pruned, so it grows with traffic. Cooldown-recovery
  probes are recorded but excluded from windows and totals. Token counts
  are presence-aware (fresh input, cache reads, cache writes, output,
  reasoning as a subset of output; NULL means the provider didn't report a
  value, 0 means it reported zero). Input is stored as fresh input for
  every provider, with cached tokens split out: OpenAI-compatible
  providers fold cached tokens into `prompt_tokens`, so NEXUS subtracts
  them, and nothing is double-counted. Rate-limit/quota headers are
  transcribed verbatim (Anthropic's unified 5h/7d utilization + reset
  epochs typed; other providers raw-captured), 429s record
  `Retry-After`/reset timestamps, and a 429 is never interpreted as proof
  of quota exhaustion. Aborted
  streams keep their observed tokens as partial usage and mark the
  request log row with the abort reason.
- **Quota windows + usage dashboard:** windows are derived at query time
  from the event history (never persisted) with honest termination
  reasons — a provider-reported reset (`PROVIDER_RESET`) is trusted over
  everything; a 429 closes the window as `RATE_LIMIT` at its own timestamp
  and is counted in that window even past the assumed 5-hour wall (it
  never opens one); otherwise the assumed wall closes it as
  `TIME_ELAPSED`; the oldest window's end reason is reported `UNKNOWN`
  instead of `TIME_ELAPSED` because its true start predates recorded
  history. Exposed via `GET /api/usage/{windows,events,totals,quota}` on
  the dashboard port plus a new "Usage windows" panel: current window
  per provider with cache-hit ratio and provider-reported utilization,
  recent window history with end reasons, and a per-provider event
  drill-down. The `/api/usage/*` endpoints return HTTP 500 with the error
  on a storage failure (instead of an empty 200); a provider's quota
  snapshot is only attached when it matches the queried dimension (so
  Anthropic's 7d figure cannot appear on a 5h card); the usage panel
  loads on page open and on its refresh button (it is not live-streamed)
  and shows a fetch failure as an error state with a retry instead of
  hiding the panel.
- **SQLite WAL mode now actually applied:** `nexus.db` really runs in WAL
  journal mode with a 5-second busy timeout; the earlier connection
  options were silently ignored by the pure-Go SQLite driver, leaving
  rollback-journal mode, which mattered once every request attempt started
  writing a usage event. Expect `nexus.db-wal` and `nexus.db-shm` next to
  `nexus.db` (in Docker, in your `./data` directory); copy all three, or
  stop NEXUS first, when backing up.

## v0.7.0

- **Host-editable config in Docker:** swaps the named Docker volume for a
  `./data` bind mount, so `config.toml` and `nexus.db` are plain files on
  the host instead of hidden inside Docker's volume storage — edit
  `config.toml` directly, no `docker exec` needed. Compose reads
  `NEXUS_UID`/`NEXUS_GID` from `.env` to match the container process to
  your own host user.
- **Sticky provider/key cooldown:** replaces per-request key round-robin
  with sticky routing — pins to one provider+key and keeps sending
  requests there until it 429s, only then rotates to the next key, and
  only advances providers once every key in the pool is cooling. Provider
  health is now derived purely from key-pool state instead of a generic
  30s `/models` ping; recovery is driven by an active background probe (a
  real minimal chat-completion request, exponential backoff 10s-5min)
  instead of a blind timer. Non-429 errors (5xx/timeout) get 2 retries
  against the same provider before falling back to the existing chain
  failover.
- **Vision / image content support:** Anthropic `image` content blocks
  (base64 or URL-sourced, including ones nested inside a `tool_result`) are
  now converted to OpenAI `image_url` content parts when a request is
  forwarded to an OpenAI-compatible provider — previously they were
  silently dropped. An image-bearing request uses the provider's normal
  mapped model unless the operator sets a one-line `vision_model` override
  per provider in `config.toml` (the same override pattern as `model_map`,
  pricing, and tier); NEXUS still doesn't maintain or infer which provider
  models support vision, and there's no filtering of the routing/cooldown
  chain by vision capability — an incompatible provider's own error is
  relayed to Claude Code unchanged, same as any other 4xx.
- **Semantic-cache fix for image-bearing requests:** the opt-in semantic
  cache (`--semantic-cache`) now skips embedding/lookup for any request
  containing image content, the same way it already skips tool-using
  requests — previously an image-bearing request could be matched against
  a cached entry keyed on an embedding that only ever saw its text.

## v0.6.0

- **OpenAI array-form content:** the chat completions gateway now accepts
  `message.content` as an array of text parts (as sent by opencode, pi.dev),
  not just a plain string — previously every such request got a 400.
- **Raw request body logged on JSON parse failure:** aids debugging malformed
  client requests instead of a bare generic 400.
- **Playground (chat UI):** a new "💬 Playground" overlay in the dashboard. Pick
  any model from any configured provider (Claude Opus, GPT-4o, DeepSeek, Groq,
  …), type a question, watch the answer stream back in real time. Every
  Playground call flows through the normal proxy pipeline so cache, cascade,
  privacy firewall and cost tracking apply for free, and the call shows up in
  the live feed. Streaming via `fetch` + `ReadableStream`, Anthropic SSE
  format. New endpoints `GET /api/playground/models` and
  `POST /api/playground/chat`. Five backend tests; verified end-to-end on the
  running binary.
- **First-run setup wizard:** open the dashboard after a fresh install and a
  4-step in-browser onboarding takes over (Welcome → Providers → Test → Connect).
  Auto-detects providers already in your env vars, recommends a sensible starter
  set, validates each key with a live `HealthCheck`, and prints a
  platform-aware (Win / macOS / Linux) copyable `ANTHROPIC_BASE_URL` + key
  snippet. Saves to `~/.nexus/config.toml`; writes a `setup-done` marker so it
  never re-appears. Skippable. New endpoints under `/api/setup/*`. Verified
  end-to-end on the running binary.
- **Licensing layer:** moved the project licence from MIT to **Apache-2.0** for
  trademark + patent-grant protection (forks may use the code but not the
  "NEXUS" name); added [`NOTICE`](NOTICE), a runtime
  [`internal/license`](internal/license/) package that's surfaced in
  `nexus version`, SPDX headers on the package entry points, and a full
  strategy doc at [`docs/LICENSING.md`](docs/LICENSING.md) — including a
  ready-to-drop BSL 1.1 template if the project ever wants newer versions to be
  source-available. Today every feature is unlocked; the seam is in place to
  gate later without touching call sites.
- **Optional Docker deployment:** a multi-stage `Dockerfile` (real dashboard
  build, distroless nonroot final image, multi-arch amd64/arm64) plus
  `docker-compose.yml` with `restart: unless-stopped` for auto-start on host
  reboot — useful for running NEXUS on a home server. Provider keys can be
  passed as plain container env vars (same auto-discovery as the binary) or
  via a one-off `docker run ... add` for custom endpoints. Tagged releases
  publish a prebuilt image to `ghcr.io/lynuxis2026-pixel/nexus-proxy`. See
  [`docs/docker.md`](docs/docker.md). Verified end-to-end: build, dashboard,
  persistence across container recreation, restart policy.

## v0.5.0

- **6 more providers (24 → 30):** Baseten, Featherless, kluster.ai, Venice
  (privacy-focused), Friendli, Chutes — all OpenAI-compatible, with env
  auto-discovery.
- **Smarter complexity classifier:** an explainable policy (security/architecture
  keywords, large-context, Opus → premium; trivial tool-less → free; ordinary
  coding/tool-use → cheap Standard) that reads intent from *user* text only.
  Routes the bulk of agentic coding to the cheap tier; full test matrix.
- **Dashboard:** a privacy "secrets masked · 0 leaked" card and a "routing mix"
  bar showing how the classifier split recent traffic.
- **Windows CI:** the test suite now runs on an ubuntu + windows matrix, plus a
  spaced-path DB-open guard.

## v0.4.1

- **Off-peak-aware pricing** — prices requests at a provider's off-peak rate
  during its UTC discount window (e.g. DeepSeek), so cost + savings reflect reality.
- **`X-Nexus-Tier` header** — pin a single call to a tier (premium for
  architecture/security, free for lint/format); lets an agent harness route per skill.
- **Agent-harness integration (ECC)** — stack-them guide, a `nexus mcp` config, and
  a proposal/PR to affaan-m/ECC.
- Launch playbook rewritten around the positioning.

## v0.4.0

- **`nexus bench`** — benchmark every provider on your *own* captured traffic
  (cost × latency × agreement) with a Provider Report Card + recommendation.
- **Adaptive routing** (`--adaptive`) — learns which provider wins each of your
  task types from real outcomes and reorders routing automatically.
- **Team mode** — per-user attribution + a shared cache + a savings leaderboard.
- **Rules engine** — declarative routing overrides in config.
- **Cost guardrail** (`--max-request-usd`) — downgrade a pricey single request.
- **Trust & Savings report** (`nexus report`) — "$ saved + N secrets masked · 0 leaked".
- Repositioned around Private · Proven · Self-learning · Local.

## v0.3.0

- **Cache-aware cost engine** — captures provider prompt-cache tokens and prices
  them at the real discount; shows a live "cache saved $X".
- **Prefix-normalized + opt-in semantic cache** (`--semantic-cache`).
- **Cheap-first cascade with verification** (`--cascade`).
- **Free-tier API key rotation / pools** with 429 cooldown.
- **Privacy firewall** (`--redact`) — mask secrets/PII before they leave; restore in
  the response.
- **Inspect & replay** (`--inspect`) — compare a captured request across providers.
- **CLI:** `nexus code`, `nexus doctor` (+ env auto-discovery), `nexus top`, `nexus mcp`.
- Budget spend alerts via Slack/Discord/generic webhook.

## v0.2.0 – v0.2.1

- 24 providers built in + a config-driven custom provider; Azure/Bedrock/Vertex
  (incl. AWS SigV4) with offline integration tests.
- **Universal gateway** — also speaks the OpenAI API (`/v1/chat/completions`).
- True OpenAI-compatible streaming, automatic failover, daily budget cap,
  normalized response cache, background health checks.
- Hardened + tested every route (httptest integration suite); fixes.

## v0.1.0

- Initial public release: single-binary proxy + live Svelte dashboard for Claude
  Code, intelligent complexity-based routing, SQLite request log + stats, and the
  first set of providers. GoReleaser cross-platform binaries.
