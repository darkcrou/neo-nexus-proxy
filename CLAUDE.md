# NEXUS — Claude Code Working Memory

## Wat is NEXUS?
Een open-source **single-binary proxy + live dashboard** voor Claude Code.
Stuurt Claude Code requests intelligent door naar goedkopere/gratis LLM providers.
Doel: viral GitHub project (target: 10k+ stars).

## Core Filosofie
- **Zero config** — `nexus start` werkt direct, geen setup vereist
- **Single binary** — één Go binary, geen dependencies, geen Docker nodig
- **Claude Code-first** — gebouwd specifiek voor Claude Code workflow
- **Beautiful by default** — dashboard dat mensen willen screenshotten

---

## Tech Stack

| Layer | Keuze | Reden |
|---|---|---|
| Proxy core | **Go** | Single binary, cross-platform, snel |
| Dashboard UI | **Svelte + Vite** | Licht, embedded in binary |
| Storage | **SQLite (modernc)** | Pure Go, geen cgo |
| Realtime | **SSE (Server-Sent Events)** | Simpel, geen WS overhead |
| Config | **TOML** | Leesbaar, simpel |
| Build | **Makefile** | Cross-platform builds |

---

## Mappenstructuur

```
nexus/
├── CLAUDE.md                    ← dit bestand
├── MEMORY.md                    ← project memory / beslissingen log
├── README.md                    ← GitHub README (viral-ready)
├── go.mod
├── go.sum
├── Makefile
├── .goreleaser.yml              ← voor GitHub releases (binaries)
├── cmd/
│   └── nexus/
│       └── main.go              ← CLI entrypoint (cobra)
├── internal/
│   ├── proxy/
│   │   ├── server.go            ← HTTP proxy server (poort 3000)
│   │   ├── handler.go           ← request interceptor
│   │   ├── transformer.go       ← Anthropic ↔ OpenAI formaat conversie
│   │   └── stream.go            ← streaming response handler
│   ├── router/
│   │   ├── router.go            ← intelligente model router
│   │   ├── classifier.go        ← task complexity classifier
│   │   └── rules.go             ← routing rules engine
│   ├── dashboard/
│   │   ├── server.go            ← dashboard HTTP server (poort 2222)
│   │   ├── sse.go               ← Server-Sent Events voor live updates
│   │   └── embed.go             ← embedded Svelte build
│   ├── storage/
│   │   ├── db.go                ← SQLite setup
│   │   ├── requests.go          ← request log opslag
│   │   └── stats.go             ← aggregated stats queries
│   └── providers/
│       ├── provider.go          ← Provider interface
│       ├── anthropic.go         ← Anthropic provider
│       ├── deepseek.go          ← DeepSeek provider
│       ├── groq.go              ← Groq provider
│       ├── gemini.go            ← Gemini provider
│       └── ollama.go            ← Ollama (lokaal) provider
├── web/                         ← Svelte dashboard source
│   ├── package.json
│   ├── vite.config.ts
│   ├── src/
│   │   ├── App.svelte
│   │   ├── main.ts
│   │   ├── components/
│   │   │   ├── RequestFeed.svelte     ← live request stream
│   │   │   ├── CostMeter.svelte       ← real-time kosten
│   │   │   ├── ModelBadge.svelte      ← welk model werd gebruikt
│   │   │   ├── ProviderStatus.svelte  ← provider health
│   │   │   └── StatsBar.svelte        ← totaal tokens/kosten
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
nexus start --port 3000 --ui 2222   # custom poorten
nexus add deepseek sk-xxx           # provider toevoegen
nexus add groq gsk-xxx
nexus add gemini AIza-xxx
nexus add ollama                    # lokaal, geen key nodig
nexus status                        # provider health check
nexus logs                          # laatste N requests
nexus cost                          # kosten overzicht
nexus config                        # open config in editor
```

---

## Proxy Endpoints (port 3000)

| Endpoint | Beschrijving |
|---|---|
| `POST /v1/messages` | Anthropic messages API (Claude Code gebruikt dit) |
| `GET /health` | Health check |

---

## Dashboard Endpoints (port 2222)

| Endpoint | Beschrijving |
|---|---|
| `GET /` | Svelte dashboard SPA |
| `GET /api/stats` | Aggregated stats JSON |
| `GET /api/requests` | Request history |
| `GET /api/providers` | Provider status |
| `GET /events` | SSE stream voor live updates |

---

## Intelligent Router Logic

```
Binnenkomende request
        ↓
   Classify task
        ↓
   ┌────────────────────────────────────┐
   │ SIMPLE  (<200 tokens, geen tools)  │ → Groq Llama (gratis)
   │ MEDIUM  (code, refactor, explain)  │ → DeepSeek V3 (~$0.001)
   │ COMPLEX (architect, debug, plan)   │ → Claude Sonnet
   │ CRITICAL(security, prod issues)    │ → Claude Opus
   └────────────────────────────────────┘
        ↓
   Fallback chain als provider faalt
        ↓
   Log naar SQLite + push naar SSE
```

### Classifier signals:
- Token count van de prompt
- Aanwezigheid van tool_use blocks
- Keywords: "architecture", "security", "production", "urgent"
- Context length van conversation history
- Aanwezigheid van code blocks

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

Claude Code vraagt altijd om een Claude model. NEXUS mapt dit:

| Claude Code vraagt | Route naar | Tier |
|---|---|---|
| `claude-opus-4-5` | Anthropic Opus / DeepSeek V3 | complex |
| `claude-sonnet-4-6` | DeepSeek / Gemini 2.0 | standard |
| `claude-haiku-4-5` | Groq Llama / Gemini Flash | free |

---

## Cost Tracking

Elke provider heeft token prijzen in `internal/providers/*.go`:

```go
type Pricing struct {
    InputPer1M  float64  // USD per 1M input tokens
    OutputPer1M float64  // USD per 1M output tokens
}
```

Kosten worden berekend per request en opgeslagen in SQLite.
Dashboard toont: per sessie, per dag, per provider, forecast voor de maand.

---

## Dashboard Design Tokens

```
Achtergrond:  #050816  (donker navy)
Surface:      #0a0e1a
Border:       #1a2035
Accent:       #7c3aed  (paars)
Accent2:      #06b6d4  (cyan)
Success:      #10b981  (groen)
Warning:      #f59e0b  (oranje)
Danger:       #ef4444  (rood)
Text:         #e2e8f0
Muted:        #64748b
Font:         'Geist Mono' voor data, 'Inter' voor tekst
```

---

## Build & Release

```makefile
# Development
make dev          # start Go proxy + Vite dev server

# Production
make build-web    # build Svelte naar web/dist/
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

## Versietags (CRUCIAAL)

**ALTIJD** actief voorstellen om een nieuwe `vX.Y.Z` tag te maken nadat een
**significante hoeveelheid werk** is afgerond en gecommit. Niet wachten tot de
gebruiker erom vraagt — dit is een proactieve suggestie, elke keer als het van
toepassing is.

Dit betekent **niet** dat elke commit een tag krijgt. Een tag hoort bij een
reeks commits die *samen* een afgeronde eenheid werk vormen, bijvoorbeeld:

- een nieuwe feature is volledig geïmplementeerd (en getest/geverifieerd)
- een majeure bug is gefixt
- een significante refactoring is afgerond
- meerdere kleinere, samenhangende commits vormen samen zo'n eenheid

Telt **niet** als trigger: een losse tussentijdse commit, werk-in-uitvoering,
of documentatie-only wijzigingen (behalve de changelog-promotie hieronder,
die hoort juist bij het taggen zelf).

Volg het bestaande patroon in de git-historie (zie bv. v0.5.0 → v0.6.0):

1. **Changelog eerst.** Zorg dat elke user-facing wijziging sinds de vorige
   tag een bullet heeft onder `## Unreleased` in `CHANGELOG.md` (commits die
   dat nog missen alsnog aanvullen).
2. **Bepaal het semver-niveau**: patch = bugfix, minor = nieuwe feature/geen
   breaking changes, major = breaking change.
3. **Promoveer** `## Unreleased` → `## vX.Y.Z` in `CHANGELOG.md` (aparte
   `release:`-commit, zoals eerder gedaan).
4. **Annotated tag**: `git tag -a vX.Y.Z -m "..."` met een korte tagline +
   bullet-samenvatting, in dezelfde stijl als bestaande tags
   (`git tag -l -n99 <laatste-tag>` om de stijl te checken).
5. **Vraag expliciet** naar welke remote(s) gepusht moeten worden
   (`opi2` / `origin` / beide) — **nooit** ongevraagd naar `origin` pushen: dat
   triggert een publieke GitHub release + Docker image publish naar ghcr.io.

---

## README Install Snippet (viral-ready)

```bash
# macOS/Linux (één commando)
curl -fsSL https://get.nexus.sh | sh

# Of direct binary
brew install nexus-proxy/nexus

# Start
nexus start
# → Proxy: http://localhost:3000
# → Dashboard: http://localhost:2222

# Koppel Claude Code
export ANTHROPIC_BASE_URL=http://localhost:3000
export ANTHROPIC_API_KEY=nexus-local
claude
```

---

## Bouwen volgorde (sprints)

### Sprint 1 — Core proxy werkend
1. `go.mod` aanmaken
2. `cmd/nexus/main.go` — cobra CLI
3. `internal/proxy/server.go` — basis HTTP server
4. `internal/proxy/handler.go` — request doorsturen naar Anthropic
5. `internal/proxy/transformer.go` — Anthropic ↔ OpenAI conversie
6. `internal/proxy/stream.go` — streaming support
7. `internal/providers/` — alle providers
8. Test: Claude Code werkt via proxy

### Sprint 2 — Router
1. `internal/storage/db.go` — SQLite setup
2. `internal/storage/requests.go` — request logging
3. `internal/router/classifier.go` — complexity classifier
4. `internal/router/router.go` — routing logic
5. `internal/router/rules.go` — fallback chains
6. Test: requests gaan naar juiste provider

### Sprint 3 — Dashboard
1. `web/` setup — Svelte + Vite
2. `internal/dashboard/sse.go` — SSE stream
3. `internal/dashboard/server.go` — dashboard API
4. Svelte components bouwen
5. `internal/dashboard/embed.go` — embed in binary
6. Test: live updates in browser

### Sprint 4 — Polish & Release
1. `Makefile` — build pipeline
2. `.goreleaser.yml` — release config
3. `README.md` — viral-ready met GIFs
4. `docs/` — documentatie
5. GitHub Actions CI/CD
6. `curl | sh` install script

---

## Kritieke beslissingen (log hier)

- **Go i.p.v. Node/Python** — single binary is de #1 viral feature
- **SQLite modernc** — geen cgo, werkt in cross-compile
- **SSE i.p.v. WebSockets** — simpeler, browser-native, minder overhead
- **Svelte i.p.v. React** — kleiner bundle, sneller, embedded beter
- **TOML config** — leesbaarder dan YAML voor eindgebruikers
- **Poort 3000 proxy, 2222 dashboard** — 2222 is memorabel, geen conflicten

---

## Wat NOOIT te doen

- Geen Python dependency toevoegen
- Geen Docker vereisen voor basis gebruik
- Geen database server (alleen SQLite)
- Geen cloud account vereisen
- Geen telemetry zonder expliciete opt-in
- Config altijd in `~/.nexus/` — nooit in project dir
