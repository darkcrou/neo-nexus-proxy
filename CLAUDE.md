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

## Provider Config (~/.nexus/config.toml)

```toml
[proxy]
port = 3000

[dashboard]
port = 2222

[routing]
strategy = "auto"   # auto | manual | cheapest | fastest

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

---

## What to NEVER Do

- Don't add a Python dependency
- Don't require Docker for basic usage
- No database server (SQLite only)
- Don't require a cloud account
- No telemetry without explicit opt-in
- Config always in `~/.nexus/` — never in the project dir
