# NEXUS — Claude Code Working Memory

## What is NEXUS?
An open-source **single-binary proxy + live dashboard** for Claude Code.
Intelligently routes Claude Code requests to cheaper/free LLM providers.
Goal: viral GitHub project (target: 10k+ stars).

## Core Philosophy
- **Zero config** — `nexus start` works immediately, no setup required
- **Single binary** — one Go binary, no dependencies, no Docker needed
- **Claude Code-first** — built specifically for the Claude Code workflow
- **Beautiful by default** — a dashboard people want to screenshot

---

## Tech Stack

| Layer | Choice | Reason |
|---|---|---|
| Proxy core | **Go** | Single binary, cross-platform, fast |
| Dashboard UI | **Svelte + Vite** | Lightweight, embedded in binary |
| Storage | **SQLite (modernc)** | Pure Go, no cgo |
| Realtime | **SSE (Server-Sent Events)** | Simple, no WS overhead |
| Config | **TOML** | Readable, simple |
| Build | **Makefile** | Cross-platform builds |

---

## Directory Structure

```
nexus/
├── CLAUDE.md                    ← this file
├── MEMORY.md                    ← project memory / decisions log
├── README.md                    ← GitHub README (viral-ready)
├── go.mod
├── go.sum
├── Makefile
├── .goreleaser.yml              ← for GitHub releases (binaries)
├── cmd/
│   └── nexus/
│       └── main.go              ← CLI entrypoint (cobra)
├── internal/
│   ├── proxy/
│   │   ├── server.go            ← HTTP proxy server (port 3000)
│   │   ├── handler.go           ← request interceptor
│   │   ├── transformer.go       ← Anthropic ↔ OpenAI format conversion
│   │   └── stream.go            ← streaming response handler
│   ├── router/
│   │   ├── router.go            ← intelligent model router
│   │   ├── classifier.go        ← task complexity classifier
│   │   └── rules.go             ← routing rules engine
│   ├── dashboard/
│   │   ├── server.go            ← dashboard HTTP server (port 2222)
│   │   ├── sse.go               ← Server-Sent Events for live updates
│   │   └── embed.go             ← embedded Svelte build
│   ├── storage/
│   │   ├── db.go                ← SQLite setup
│   │   ├── requests.go          ← request log storage
│   │   └── stats.go             ← aggregated stats queries
│   └── providers/
│       ├── provider.go          ← Provider interface
│       ├── anthropic.go         ← Anthropic provider
│       ├── deepseek.go          ← DeepSeek provider
│       ├── groq.go              ← Groq provider
│       ├── gemini.go            ← Gemini provider
│       └── ollama.go            ← Ollama (local) provider
├── web/                         ← Svelte dashboard source
│   ├── package.json
│   ├── vite.config.ts
│   ├── src/
│   │   ├── App.svelte
│   │   ├── main.ts
│   │   ├── components/
│   │   │   ├── RequestFeed.svelte     ← live request stream
│   │   │   ├── CostMeter.svelte       ← real-time costs
│   │   │   ├── ModelBadge.svelte      ← which model was used
│   │   │   ├── ProviderStatus.svelte  ← provider health
│   │   │   └── StatsBar.svelte        ← total tokens/costs
│   │   ├── stores/
│   │   │   ├── requests.ts            ← SSE store
│   │   │   └── stats.ts               ← stats store
│   │   └── pages/
│   │       ├── Dashboard.svelte
│   │       ├── Requests.svelte
│   │       └── Settings.svelte
│   └── public/
└── docs/
    ├── architecture.md
    ├── providers.md
    └── routing.md
```

---

## CLI Interface (cobra)

```bash
nexus start                         # start proxy + dashboard
nexus start --port 3000 --ui 2222   # custom ports
nexus add deepseek sk-xxx           # add provider
nexus add groq gsk-xxx
nexus add gemini AIza-xxx
nexus add ollama                    # local, no key needed
nexus status                        # provider health check
nexus logs                          # last N requests
nexus cost                          # cost overview
nexus config                        # open config in editor
```

---

## Proxy Endpoints (port 3000)

| Endpoint | Description |
|---|---|
| `POST /v1/messages` | Anthropic messages API (used by Claude Code) |
| `GET /health` | Health check |

---

## Dashboard Endpoints (port 2222)

| Endpoint | Description |
|---|---|
| `GET /` | Svelte dashboard SPA |
| `GET /api/stats` | Aggregated stats JSON |
| `GET /api/requests` | Request history |
| `GET /api/providers` | Provider status |
| `GET /api/usage/windows` | Derived quota windows (per provider, most recent first) |
| `GET /api/usage/events` | Raw usage events (newest first, filterable) |
| `GET /api/usage/totals` | Usage totals grouped by provider or model |
| `GET /api/usage/quota` | Latest provider-reported quota utilization |
| `GET /events` | SSE stream for live updates |

---

## Intelligent Router Logic

```
Incoming request
        ↓
   Classify task
        ↓
   ┌────────────────────────────────────┐
   │ SIMPLE  (<200 tokens, no tools)    │ → Groq Llama (free)
   │ MEDIUM  (code, refactor, explain)  │ → DeepSeek V3 (~$0.001)
   │ COMPLEX (architect, debug, plan)   │ → Claude Sonnet
   │ CRITICAL(security, prod issues)    │ → Claude Opus
   └────────────────────────────────────┘
        ↓
   Fallback chain if provider fails
        ↓
   Log to SQLite + push to SSE
```

### Classifier signals:
- Token count of the prompt
- Presence of tool_use blocks
- Keywords: "architecture", "security", "production", "urgent"
- Context length of conversation history
- Presence of code blocks

---

## Provider & Key Cooldown (two-level, sticky)

Implemented in `feat(proxy): two-level sticky provider/key cooldown`
(commit `548f736`). All of this lives in `internal/proxy/handler.go` unless
noted otherwise. Read this before touching provider selection, 429
handling, or provider health — it replaced the old naive round-robin +
generic health-ping design entirely.

**The model:** two independent cooldown levels, where level 2 is *derived*
from level 1, not an independent timer.

- **Level 1 — key cooldown**, on `activeProvider` (a provider + its pool of
  `api_keys`): `pickKey() (key, idx, ok)` is **sticky**, not round-robin —
  it returns the same key on every call until that key 429s, only then
  scanning forward to the next non-cooling key of the *same* provider.
  `hasAvailableKey()` reports whether any key is usable right now. If
  *every* key in the pool is cooling, `pickKey` returns `ok=false` and
  `callUpstream` returns the sentinel `errProviderExhausted` instead of
  trying a known-dead key.
- **Level 2 — provider cooldown**, is **not** a separate timer. A provider
  is available exactly when it has ≥1 non-cooling key. `coolKey`/
  `recoverKey` keep `router.Provider.Healthy` (read by `RouteChain`)
  in sync with real key state — `internal/router/router.go` itself was
  **not modified**; it just gets fed a different, more accurate signal.
- **Recovery is by active probe, not blind timer expiry.** `keyProbeLoop`
  (background, replaces the old 30s generic `healthLoop`) periodically
  calls `probeCoolingKeys()` → `probeKey()`, which fires one real minimal
  chat-completion request (`MaxTokens: 1`, "ping") at a *specific cooling
  key's actual endpoint* via `callUpstreamOnce` — not a cheap `/models`
  ping, because a `/models` check can succeed while the chat-completions
  endpoint that actually 429'd is still limited. A non-429 response clears
  the key's cooldown immediately. Each key has its own exponential backoff
  (`reschedule`: starts at 10s, doubles, capped at 5 min) so a key that's
  out on a real daily-quota exhaustion isn't hammered for hours.
  **`Provider.HealthCheck()`** (the interface method, generic `/models` or
  reachability ping) still exists and is still used — but only for
  one-off CLI/UI diagnostics (`nexus status`, `nexus doctor`, dashboard
  onboarding in `internal/dashboard/setup.go`), never for routing-time
  availability anymore.
- **Sticky provider selection**, scoped **per requested Claude model**
  (e.g. "claude-sonnet-4-6"), not global and not per-complexity:
  `Handler.sticky map[string]string` + `stickyProvider()`/`setSticky()`,
  and the pure helper `stickyReorder(chain, sticky)` which reprioritizes
  `RouteChain`'s output without changing `RouteChain` itself. Updated only
  on an actual successful response, from whichever provider produced it
  (429-driven switch or 5xx-retry-exhaustion alike — one success-path
  write site). Known, intentional nuance: under `StrategyAuto` (default),
  `RouteChain` reorders by classified complexity, not by requested model —
  so the same model string can occasionally land in a different chain
  across requests, and the sticky pointer just won't be found that time.
  This is graceful degradation, not a bug.
- **Non-429 errors are a separate, unrelated mechanism**: 500/502/503/504/
  transport errors get up to `maxProviderAttempts = 3` total attempts
  against the *same* provider (no backoff, tight retry), then fall through
  to the pre-existing next-provider chain-walk (`isRetryableStatus`)
  exactly as before. This never touches cooldown state — cooldown is
  strictly a 429/rate-limit concept.

**Design precedent worth repeating:** the whole rework touched only
`internal/proxy/handler.go` (+ one `pickKey` call site in `gateway.go`) and
added zero changes to `internal/router/router.go` — existing plumbing
(`Healthy`/`SetHealthy`/`RouteChain`) was reused by feeding it a better
signal, rather than building a parallel mechanism. Prefer that shape again
for similar changes.

---

## Vision / Image Content Support

Read this before touching image content handling, vision-model config, or
the semantic cache's request-skip logic. Spans
`internal/proxy/transformer.go`, `internal/providers/generic.go`,
`internal/config/config.go`, `internal/proxy/handler.go`,
`internal/proxy/semantic.go`, and `internal/proxy/gateway.go`.

- **Content conversion** (`transformer.go`): `convertMessage` builds an
  ordered list of OpenAI content parts via `blocksToParts` — Anthropic
  `"text"` blocks become `{"type":"text"}` parts, `"image"` blocks become
  `{"type":"image_url"}` parts via `imagePart` (a `base64` source becomes a
  `data:` URI, a `url` source passes through verbatim), and an image nested
  inside a `tool_result`'s own `content` array is spliced in by recursing
  into `blocksToParts`. `hasImagePart` then decides the final shape: if no
  image is present anywhere, the parts collapse back to the legacy flat
  string (`joinText`) so text-only requests still marshal byte-identically
  to before this feature existed; only when an image is present does
  `OpenAIContent` marshal as an array (`partsContent`/`asArray`). The
  Anthropic-native passthrough path (anthropic, bedrock, vertex) forwards
  raw request bytes unchanged and needed no change.
- **Operator-supplied vision-model override, not a NEXUS-maintained
  catalog** (`config.go`/`generic.go`): a new optional `vision_model` TOML
  key on `Provider` flows into `providers.Spec.VisionModel`, read through
  the optional `VisionCapable` interface (`VisionModel(claudeModel string)
  string`) implemented identically by `*overridden` (a built-in provider
  with config overrides applied) and `*Generic` (a fully config-driven
  custom/OpenAI-compatible provider) — the same override mechanism already
  used for `model_map`, pricing, and tier. An empty return means "no
  override": NEXUS has no built-in notion of which model IDs support
  vision for any provider.
- **Handler wiring** (`handler.go`): `hasImageContent` — a shallow,
  top-level-only scan over the raw parsed messages (it does not recurse
  into `tool_result`, unlike the transformer's full conversion) — sets a
  new unexported, per-request `AnthropicRequest.nexusImages` field (same
  pattern as `nexusUser`/`nexusRedacted`: derived per request, never
  serialized). In `callUpstreamOnce`, when `nexusImages` is true and the
  active provider implements `VisionCapable` with a non-empty
  `VisionModel(req.Model)`, that value replaces `MapModel(req.Model)` as
  the `targetModel` passed to `TransformToOpenAI` — otherwise behavior is
  completely unchanged from before this feature.
- **No chain filtering, by design:** `nexusImages` never reaches
  `RouteChain`, the sticky-provider logic, or the cooldown machinery — an
  image-bearing request walks the exact same provider chain a text-only
  request would. If the provider NEXUS picks can't actually handle the
  image, its own error response is relayed to Claude Code as-is, identical
  to how any other provider 4xx is already relayed today — an honest
  failure, not a NEXUS-side guess.
- **Semantic-cache exclusion** (`semantic.go`): `promptText` now also
  returns `hasImages`, computed by a sibling scan `hasImageBlock` that
  walks the same `system`/message-`content` shape `collectText` already
  walks. Both call sites — `HandleMessages` (`handler.go`) and
  `HandleChatCompletions` (`gateway.go`, the OpenAI-compatible
  `/v1/chat/completions` gateway) — skip the semantic embed/lookup when
  `hasImages` is true, mirroring the existing `hasTools` skip: the
  embedding is a text-only hashed sparse vector, so an image-bearing
  request has no business being embedded or matched against one.

**Why there's no hardcoded vision-capable-model catalog.** The original
plan for this feature required NEXUS to maintain a hardcoded, per-provider
table of "current vision-capable model IDs," confirmed by live doc
research. That attempt was abandoned mid-implementation: live research
produced internally contradictory results for 3 of 4 target providers
(only one came back consistent). The deeper problem it surfaced wasn't the
research quality — it's that maintaining such a catalog fights what NEXUS
actually is: a **sticky forwarder** over an operator-configured provider
list (see "Provider & Key Cooldown" above), not a content-aware router
that curates a model catalog on the operator's behalf. A shadow copy of
"which model IDs currently support vision" in Go source goes stale the
moment any provider ships a new model, and duplicates configuration the
operator already owns. **Do not redo that research or reintroduce a
built-in vision-model catalog** — if a future session is tempted to, this
paragraph is why it was dropped, not an oversight to fix.

---

## Direct Strategy (Model-ID Passthrough)

Read this before touching routing-strategy selection or OpenAI-compatible
model-id forwarding. Spans `internal/router/router.go`,
`internal/proxy/handler.go`, `internal/proxy/gateway.go`, and
`internal/proxy/openai.go`.

- **What it does** (`router.go`): `StrategyDirect` (line 45) is a new
  `RoutingStrategy` value. Its `RouteChain` case (lines 192-193) reuses
  `autoChain(complexity)` — the exact same chain-ordering, sticky-provider
  selection, and key/provider cooldown machinery as `auto`, unchanged. The
  only thing `direct` changes is the model id NEXUS forwards to
  OpenAI-compatible providers: `Handler.directModel` (`handler.go` line
  192) is set once in `NewHandler` (line 320:
  `h.directModel = router.RoutingStrategy(appCfg.Routing.Strategy) ==
  router.StrategyDirect`). The helper `mappedModel` (`handler.go` lines
  918-923) returns `requestedModel` verbatim when `directModel` is set,
  else falls back to `active.impl.MapModel(requestedModel)`; it's used for
  the OpenAI passthrough gateway (`callOpenAIPassthrough`, `gateway.go`
  line 183) and for the log/dashboard `model_used` value (`openai.go` line
  62, `handler.go` line 1123, `logResult`). The request-shaping branch
  itself lives in `callUpstreamOnce` (`handler.go` lines 926-938): inside
  the `providers.IsOpenAICompatible` block, `targetModel` starts as
  `req.Model` and, when `h.directModel` is true, skips the entire
  `mappedModel`/`nexusImages`/`VisionCapable` block below it — one branch
  bypasses `MapModel` and the `vision_model` override together, not two
  separate opt-outs.
- **Why it exists**: `Generic.MapModel` (`internal/providers/generic.go`
  lines 186-193) silently substitutes `g.models[0]` (the provider's first
  configured model) for any Claude model id it can't find in `model_map`.
  That's the right default for Claude Code's own model names, but it means
  an operator who wants NEXUS to address a specific, non-mapped provider
  model literally — e.g. `glm-5.2` — had no way to get that exact string
  forwarded; it was always silently remapped instead. `direct` exists
  purely to disable that substitution.
- **Unconditional passthrough, no exceptions — including images.**
  Confirmed with the user rather than assumed: under `direct`, an
  image-bearing request does **not** get the `vision_model` override
  either, unlike `auto` (where `nexusImages` + `VisionCapable.VisionModel`
  can replace the mapped model — see "Vision / Image Content Support"
  above). Under `direct` the requested model id always wins, image or not.
- **Scope boundary.** This only touches OpenAI-compatible providers
  (`providers.IsOpenAICompatible`, `internal/providers/provider.go` —
  everything except `anthropic`, `bedrock`, `vertex`). The Anthropic-native
  and enterprise (Bedrock/Vertex) passthrough paths forward raw request
  bytes with their own unrelated, provider-specific model addressing and
  are untouched by this flag.
- **Dashboard is unaffected.** `router.ClassifyRequest` (called
  unconditionally at `handler.go` line 676 and `gateway.go` line 98) runs
  before any strategy branching, because its output still drives
  `autoChain`'s tier ordering under `direct` too. So the "Live request
  feed" complexity badge (`web/src/App.svelte` line 528, `req.complexity`,
  sourced from `storage.Request.Complexity`) keeps showing the same
  simple/standard/complex/critical value regardless of strategy.

---

## Usage Measurement (immutable events + quota windows)

Read this before touching usage parsing, the `usage_events` table, the
attempt-recording hooks in the proxy hot path, or the `/api/usage/*`
endpoints. Spans `internal/storage/db.go` (schema),
`internal/storage/usage.go` (queries), `internal/proxy/usage.go`
(parsing), `internal/proxy/handler.go` / `openai.go` / `stream.go` /
`gateway.go` / `cascade.go` (recording hooks), `internal/dashboard/usage.go`
(API), and the web usage panel.

- **One immutable event per upstream attempt.** Every roundtrip that leaves
  the machine — chain failover steps, 429 key rotations, cascade candidates,
  cooldown-recovery probes, and streams that aborted mid-flight — appends one
  row to `usage_events` (SQLite; single-connection, concurrency-safe writes
  via the existing storage conventions). Rows are never updated or deleted;
  nothing is reset at window boundaries. The `requests` table is untouched
  except that aborted streams additionally set its previously-never-populated
  `Error` column to the same abort text.
- **Presence-aware token counts.** `in_tokens`, `out_tokens`,
  `cache_read_tokens`, `cache_write_tokens`, `reasoning_tokens` are all
  **nullable**: NULL means the provider did not report it, 0 means the
  provider reported zero — never collapse the two, never fabricate. Fresh
  input is `prompt − cached` (clamped at 0) so input and cache-read never
  double-count. Reasoning tokens remain a subset of `out_tokens` (never
  added to totals). Parsers: `anthropicRawUsage` (JSON), `streamRawUsage`
  (SSE regex, last `output_tokens` wins), `openAIRawUsage` (DeepSeek
  `prompt_cache_hit_tokens`, OpenAI `cached_tokens`, `reasoning_tokens`
  inside `completion_tokens_details`).
- **Quota is transcribed, never computed.** Response headers are captured
  by prefix (`anthropic-ratelimit-`, `x-ratelimit-`, `x-quota-`,
  `x-ollama-`, `ratelimit-`, `x-remaining-`, `retry-after`) into
  `quota_meta` raw JSON; the Anthropic unified family additionally
  transcribes typed fields (`quota_dimension` "5h"/"7d",
  `quota_utilization` exactly as reported, `quota_reset_at`). A 429 sets
  `rate_limited=1` + `retry_after` (seconds) + `retry_reset_at`
  (RFC3339) — and is **never** treated as proof of 5h-quota exhaustion
  unless the provider's own headers say so.
- **Windows are derived at query time**, never persisted
  (`GetUsageWindows`). Probes are excluded (internal recovery pings, not
  workload). Rate-limited events **close** the current window as
  `RATE_LIMIT` — at the 429's own timestamp, which outranks an
  already-passed duration wall (the 429 belongs to the window it
  terminates; the flipped order would drop it from every window) — but
  never open one. A provider-reported `quota_reset_at`
  (matching the queried dimension) closes as `PROVIDER_RESET` — trusted
  over assumed walls. Otherwise the assumed duration wall (default 5h)
  closes as `TIME_ELAPSED`; the oldest window's start-relative
  `TIME_ELAPSED` is downgraded to `UNKNOWN` (its true start predates
  recorded history). The current window is open (`ended_at` absent).
- **Scale characteristic, accepted by design:** `GetUsageWindows` scans a
  provider's full event history (per dashboard windows call, per provider)
  because events are never pruned — the immutable history IS the point of
  the subsystem. Rows are tiny and a single-user proxy accumulates them
  slowly, so this stays cheap for the intended lifetime of a local install.
  If it ever matters, the fix is SQL-side windowed aggregation — NOT a
  time-bounded event load, which would silently truncate the oldest
  returned window's token totals (`UNKNOWN` covers end reasons, not
  missing data).
- **API shape** (`internal/dashboard/usage.go`, all plural-envelope
  snake_case, empty arrays on nil-db): `GET /api/usage/windows`,
  `/api/usage/events`, `/api/usage/totals`, `/api/usage/quota`.
- **Provider limitations** (why fields are NULL, not bugs): Z.ai does not
  expose the account credit balance in API responses (only on the plan
  web pages) → no quota observations. Ollama Cloud sends no quota or
  rate-limit headers on responses (ollama/ollama#15663) → none captured.
  Anthropic reports the fullest picture: unified quota-status headers
  (5h/7d utilization + reset epochs) plus per-model API-key
  `anthropic-ratelimit-*` limits (raw-captured).

---

## Provider Config (~/.nexus/config.toml)

```toml
[proxy]
port = 3000

[dashboard]
port = 2222

[routing]
strategy = "auto"   # auto | manual | cheapest | fastest | direct

[[providers]]
name = "anthropic"
api_key = "sk-ant-..."
models = ["claude-opus-4-5", "claude-sonnet-4-6", "claude-haiku-4-5"]
tier = "premium"

[[providers]]
name = "deepseek"
api_key = "sk-..."
models = ["deepseek-chat", "deepseek-coder"]
tier = "standard"

[[providers]]
name = "groq"
api_key = "gsk-..."
models = ["llama-3.3-70b-versatile", "mixtral-8x7b"]
tier = "free"

[[providers]]
name = "ollama"
base_url = "http://localhost:11434"
models = ["codellama:13b"]
tier = "local"
```

---

## Model Mapping (Claude Code → Provider)

Claude Code always requests a Claude model. NEXUS maps this:

| Claude Code requests | Routes to | Tier |
|---|---|---|
| `claude-opus-4-5` | Anthropic Opus / DeepSeek V3 | complex |
| `claude-sonnet-4-6` | DeepSeek / Gemini 2.0 | standard |
| `claude-haiku-4-5` | Groq Llama / Gemini Flash | free |

---

## Cost Tracking

Each provider has token prices in `internal/providers/*.go`:

```go
type Pricing struct {
    InputPer1M  float64  // USD per 1M input tokens
    OutputPer1M float64  // USD per 1M output tokens
}
```

Costs are calculated per request and stored in SQLite.
Dashboard shows: per session, per day, per provider, forecast for the month.

---

## Dashboard Design Tokens

```
Background:   #050816  (dark navy)
Surface:      #0a0e1a
Border:       #1a2035
Accent:       #7c3aed  (purple)
Accent2:      #06b6d4  (cyan)
Success:      #10b981  (green)
Warning:      #f59e0b  (orange)
Danger:       #ef4444  (red)
Text:         #e2e8f0
Muted:        #64748b
Font:         'Geist Mono' for data, 'Inter' for text
```

---

## Build & Release

```makefile
# Development
make dev          # start Go proxy + Vite dev server

# Production
make build-web    # build Svelte to web/dist/
make embed        # embed web/dist/ in Go binary
make build        # compile nexus binary
make release      # GoReleaser → GitHub Release
```

### Cross-platform targets:
- `nexus-linux-amd64`
- `nexus-linux-arm64`
- `nexus-darwin-amd64`
- `nexus-darwin-arm64`
- `nexus-windows-amd64.exe`

---

## Version Tags (CRUCIAL)

**ALWAYS** actively suggest creating a new `vX.Y.Z` tag after a
**significant amount of work** has been completed and committed. Don't wait
for the user to ask — this is a proactive suggestion, every time it
applies.

This does **not** mean every commit gets a tag. A tag belongs to a series
of commits that *together* form a completed unit of work, for example:

- a new feature has been fully implemented (and tested/verified)
- a major bug has been fixed
- a significant refactoring has been completed
- multiple smaller, related commits together form such a unit

Does **not** count as a trigger: a single intermediate commit, work in
progress, or documentation-only changes (except the changelog promotion
below, which is itself part of the tagging process).

Follow the existing pattern in the git history (see e.g. v0.5.0 → v0.6.0):

1. **Changelog first.** Make sure every user-facing change since the
   previous tag has a bullet under `## Unreleased` in `CHANGELOG.md` (add
   it retroactively for commits still missing one).
2. **Determine the semver level**: patch = bugfix, minor = new
   feature/no breaking changes, major = breaking change.
3. **Promote** `## Unreleased` → `## vX.Y.Z` in `CHANGELOG.md` (separate
   `release:` commit, as done before).
4. **Annotated tag**: `git tag -a vX.Y.Z -m "..."` with a short tagline +
   bullet summary, in the same style as existing tags
   (`git tag -l -n99 <latest-tag>` to check the style).
5. **Explicitly ask** which remote(s) to push to
   (`opi2` / `origin` / both) — **never** push to `origin` without being
   asked: that triggers a public GitHub release + Docker image publish to
   ghcr.io.

---

## README Install Snippet (viral-ready)

```bash
# macOS/Linux (one command)
curl -fsSL https://get.nexus.sh | sh

# Or direct binary
brew install nexus-proxy/nexus

# Start
nexus start
# → Proxy: http://localhost:3000
# → Dashboard: http://localhost:2222

# Connect Claude Code
export ANTHROPIC_BASE_URL=http://localhost:3000
export ANTHROPIC_API_KEY=nexus-local
claude
```

---

## Build Order (sprints)

### Sprint 1 — Core proxy working
1. Create `go.mod`
2. `cmd/nexus/main.go` — cobra CLI
3. `internal/proxy/server.go` — basic HTTP server
4. `internal/proxy/handler.go` — forward request to Anthropic
5. `internal/proxy/transformer.go` — Anthropic ↔ OpenAI conversion
6. `internal/proxy/stream.go` — streaming support
7. `internal/providers/` — all providers
8. Test: Claude Code works via proxy

### Sprint 2 — Router
1. `internal/storage/db.go` — SQLite setup
2. `internal/storage/requests.go` — request logging
3. `internal/router/classifier.go` — complexity classifier
4. `internal/router/router.go` — routing logic
5. `internal/router/rules.go` — fallback chains
6. Test: requests go to the correct provider

### Sprint 3 — Dashboard
1. `web/` setup — Svelte + Vite
2. `internal/dashboard/sse.go` — SSE stream
3. `internal/dashboard/server.go` — dashboard API
4. Build Svelte components
5. `internal/dashboard/embed.go` — embed in binary
6. Test: live updates in browser

### Sprint 4 — Polish & Release
1. `Makefile` — build pipeline
2. `.goreleaser.yml` — release config
3. `README.md` — viral-ready with GIFs
4. `docs/` — documentation
5. GitHub Actions CI/CD
6. `curl | sh` install script

---

## Critical Decisions (log here)

- **Go instead of Node/Python** — single binary is the #1 viral feature
- **SQLite modernc** — no cgo, works in cross-compile
- **SSE instead of WebSockets** — simpler, browser-native, less overhead
- **Svelte instead of React** — smaller bundle, faster, embeds better
- **TOML config** — more readable than YAML for end users
- **Port 3000 proxy, 2222 dashboard** — 2222 is memorable, no conflicts
- **Two-level sticky cooldown (key, then provider) replaced round-robin +
  generic health-ping** — pin to one provider+key until it 429s; provider
  availability is derived from key state, not an independent timer;
  recovery is by active probe against the real endpoint, not blind timer
  expiry. See "Provider & Key Cooldown" section above.
- **No hardcoded vision-capable-model catalog; `vision_model` is a plain
  operator override instead** — an initial per-provider vision-model table
  (confirmed by live doc research) was abandoned after that research came
  back internally contradictory for most target providers, and because a
  Go-source shadow copy of provider model catalogs conflicts with NEXUS's
  role as a sticky forwarder, not a content-aware router. There is no
  vision-based filtering of the routing/cooldown chain. See "Vision / Image
  Content Support" section above.
- **`direct` routing strategy: unconditional model-id passthrough, no
  vision-override exception** — `Generic.MapModel` silently substitutes the
  provider's first configured model for any unrecognized Claude model id,
  so there was previously no way to address a specific provider model
  (e.g. `glm-5.2`) literally. `direct` reuses `auto`'s chain ordering
  (`autoChain`) unchanged but forwards the client's requested model id
  verbatim to OpenAI-compatible providers, bypassing both `model_map` and
  `vision_model`. Confirmed with the user that there are no exceptions,
  including for image requests. See "Direct Strategy (Model-ID
  Passthrough)" section above.
- **Usage measurement: immutable per-attempt events + query-time windows,
  presence-aware NULLs, transcribed (never computed) quota** — the
  subsystem records one append-only `usage_events` row per upstream
  attempt (failover step, key rotation, cascade candidate, probe, aborted
  stream); token fields are nullable where NULL = "provider didn't
  report" (0 = reported zero); quota/rate-limit headers are transcribed
  verbatim (Anthropic unified family typed, others raw-captured JSON);
  429s are never interpreted as quota exhaustion; windows are derived at
  query time with termination reasons (TIME_ELAPSED / RATE_LIMIT /
  PROVIDER_RESET / UNKNOWN) and provider-reported resets preferred over
  assumed 5h walls; aborted streams record partial usage (`usage_partial=1`)
  and mark the requests row's `Error` column; probes are recorded but
  excluded from windows/totals. See "Usage Measurement" section above.

---

## What to NEVER Do

- Don't add a Python dependency
- Don't require Docker for basic usage
- No database server (SQLite only)
- Don't require a cloud account
- No telemetry without explicit opt-in
- Config always in `~/.nexus/` — never in the project dir
