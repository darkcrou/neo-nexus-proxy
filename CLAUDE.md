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
├── CHANGELOG.md                 ← user-facing changes; promote Unreleased → vX.Y.Z when tagging
├── .goreleaser.yml              ← for GitHub releases (binaries)
├── Dockerfile
├── docker-compose.yml           ← optional container path (see docs/docker.md)
├── install.sh / install.ps1     ← `curl | sh` installer
├── .github/workflows/ci.yml     ← test (linux+windows) → release (on v* tag) → docker
├── cmd/
│   └── nexus/
│       └── main.go              ← CLI entrypoint (cobra)
├── internal/
│   ├── proxy/                   ← the request path (15 files)
│   │   ├── server.go            ← mux + routes (port 3000)
│   │   ├── handler.go           ← HandleMessages: classify → chain → cooldown → upstream (~1.7k lines)
│   │   ├── gateway.go           ← HandleChatCompletions (OpenAI in) + HandleModels (/v1/models)
│   │   ├── catalog.go           ← model catalog: live discovery + TTL cache
│   │   ├── transformer.go       ← Anthropic ↔ OpenAI format conversion
│   │   ├── stream.go            ← streaming response handler
│   │   └── cascade.go, semantic.go, cache.go, firewall.go, images.go,
│   │       rules.go, usage.go, openai.go
│   ├── router/
│   │   ├── router.go            ← RouteChain, PreferModel, model partition
│   │   └── classifier.go        ← task complexity classifier
│   ├── providers/               ← ~30 files, one per provider family
│   │   ├── provider.go          ← Provider interface, DefaultModels, health check
│   │   ├── generic.go           ← config-driven provider + `overridden` wrapper
│   │   ├── models.go            ← model discovery (per-family recipes)
│   │   └── anthropic.go, openai.go, gemini.go, ollama.go, deepseek.go, …
│   ├── config/
│   │   └── config.go            ← Default/Load, DiscoverFromEnv, EnvKeyVars
│   ├── storage/
│   │   ├── db.go                ← SQLite schema + setup
│   │   ├── requests.go          ← request log storage
│   │   └── usage.go             ← usage events + query-time windows
│   ├── dashboard/
│   │   ├── server.go            ← dashboard HTTP server (port 2222)
│   │   ├── sse.go               ← Server-Sent Events for live updates
│   │   ├── usage.go, playground.go, setup.go
│   │   ├── embed.go             ← embeds dist/
│   │   └── dist/                ← TRACKED build output (web/dist/ is gitignored)
│   └── license/                 ← community-edition gating
├── web/                         ← Svelte source (build output → internal/dashboard/dist)
│   ├── package.json
│   ├── vite.config.ts
│   └── src/
│       ├── App.svelte           ← the whole dashboard: one ~1000-line component, no pages/
│       ├── main.ts
│       ├── components/UsageWindowDetails.svelte
│       └── stores/{requests,usage}.ts
├── tools/herogif/              ← README asset generator
└── docs/
    ├── architecture.md, docker.md, agent-harnesses.md, LICENSING.md, MOBILE.md
    ├── plans/                  ← task-plan documents
    ├── reports/                ← completion reports
    └── tickets/                ← ticketing-CLI items
```

---

## Where to start (navigation)

Read this before grepping. Task → file:

| Working on… | Start at |
|---|---|
| A request/response bug | `internal/proxy/handler.go` (`HandleMessages`), then `stream.go` / `transformer.go` |
| The OpenAI gateway | `internal/proxy/gateway.go` (`HandleChatCompletions`, `HandleModels`) |
| Provider selection, cooldown, stickiness | `handler.go` (`pickKey`/`coolKey`/`stickyProvider`) + `internal/router/router.go` (`RouteChain`, `PreferModel`) |
| A provider's wire format | `internal/providers/<family>.go`; shared config-driven logic in `generic.go` |
| Config keys / env discovery | `internal/config/config.go` (`Default`, `Load`, `DiscoverFromEnv`, `EnvKeyVars`) |
| Schema or stats | `internal/storage/db.go` (schema), `usage.go` (windows/totals) |
| Dashboard API | `internal/dashboard/server.go`; the UI is `web/src/App.svelte` (one file) |
| CLI commands | `cmd/nexus/main.go` (cobra) |

Corrections to older revisions of this file: `docs/providers.md`,
`docs/routing.md`, `internal/router/rules.go`, `internal/storage/stats.go`,
`web/src/pages/*` and the five per-widget Svelte components were never written
(or were consolidated away). If you find them referenced elsewhere, that
reference is stale — don't go looking for the files.

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
| `GET /v1/models` | OpenAI-shaped model list (the catalog snapshot; no discovery, no auth) |
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

Both chat endpoints share key rotation and per-model provider stickiness.
HTTP 402 and 429 put the responding configured key on cooldown. NEXUS first tries
another usable key on the same provider. When the last key cools, the
provider becomes unavailable for every model it serves. The current
request can fail over to other eligible providers.

`keyProbeLoop` checks due keys every five seconds. It sends a small chat
request using the model associated with the cooldown. A successful probe
restores the key and makes its provider eligible again. Failed probes
double the delay from the initial 10 seconds up to five minutes. A key
stays out of normal rotation until a probe succeeds. If the upstream
rejects the probe model, the checker can try another advertised model.
Client-supplied Anthropic credentials travel with the requesting client.

`Provider.HealthCheck()` serves CLI and dashboard diagnostics. Routing
recovery probes the chat endpoint because a model listing can succeed
while the key is still out of chat quota.

Both endpoints remember the last provider to serve each requested model
successfully. If the original provider recovers, requests stay on the
working fallback while it remains eligible. Provider recovery and model
discovery leave the sticky selection alone. The Anthropic endpoint also
retries transient transport and 5xx errors up to three times within a
provider before trying the next one.

---

## Vision / Image Content Support

Read this before touching image content handling, vision-model config, the
semantic cache's request-skip logic, the privacy firewall's treatment of
image payloads, or inspector prompt capture. Spans
`internal/proxy/transformer.go`, `internal/proxy/images.go`,
`internal/providers/generic.go`, `internal/config/config.go`,
`internal/proxy/handler.go`, `internal/proxy/semantic.go`,
`internal/proxy/firewall.go`, and `internal/proxy/gateway.go`.
Images are handled on both inbound paths: `/v1/messages` (Anthropic shape)
and `/v1/chat/completions` (OpenAI shape, `gateway.go`).

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
  `OpenAIContent` marshal as an array (`partsContent`/`asArray`). On
  `/v1/messages`, the Anthropic-native passthrough path (anthropic,
  bedrock, vertex) forwards raw request bytes unchanged and needed no
  change (the gateway's OpenAI-to-Anthropic direction is covered below).
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
- **One shared, recursive image detector** (`images.go`):
  `contentHasImage` / `messagesHaveImage` replaced the earlier per-site
  scans (`hasImageContent` in `handler.go`, which only looked at top-level
  blocks and so missed images nested in a `tool_result`, and
  `hasImageBlock` in `semantic.go`, which only matched the Anthropic
  `"image"` type). It matches Anthropic `"image"` and OpenAI `"image_url"`
  / `"input_image"` blocks, and recurses into a block's own `content`
  array (an Anthropic `tool_result`) up to `maxImageScanDepth` (8) levels.
  Recursion matters because that is how Claude Code delivers an image it
  read from disk. Plain-string content is never an image. Every consumer
  (the `nexusImages` flag on both inbound paths, and the semantic-cache
  skip) goes through it, so the sites cannot drift apart again.
- **Handler wiring** (`handler.go`, `gateway.go`): `messagesHaveImage` sets
  the unexported, per-request `AnthropicRequest.nexusImages` field (same
  pattern as `nexusUser`/`nexusRedacted`: derived per request, never
  serialized) in `HandleMessages` and in `HandleChatCompletions`. The model
  id actually sent upstream comes from one function, `upstreamModel(active,
  requestedModel, images)`, used by `callUpstreamOnce` (`/v1/messages`),
  by `callOpenAIPassthrough` (`/v1/chat/completions` raw passthrough), and
  by every log/usage record (`model_used`), so the recorded model always
  names the model that was really sent. Order: `StrategyDirect` returns the
  requested id verbatim; else, if `images` is true, the provider is
  OpenAI-compatible (`providers.IsOpenAICompatible`) and implements
  `VisionCapable` with a non-empty `VisionModel(requestedModel)`, that
  value is used; else `mappedModel`. The vision override is applied only
  for OpenAI-compatible providers: the Anthropic-native paths (anthropic,
  bedrock, vertex) forward the model id unchanged and never consult
  `vision_model`. `mappedModel` still exists and stays image-unaware (it is
  `upstreamModel`'s fallback); do not call it directly for a model id that
  is sent upstream or logged.
- **Gateway inbound `image_url` support** (`transformer.go`,
  `OpenAIContent.UnmarshalJSON`): `/v1/chat/completions` content arrays
  accept `"text"` and `"image_url"` parts (previously every non-text part
  was a 400). The `image_url` part is validated strictly at parse time,
  against the shapes the OpenAI spec defines: an `http://` or `https://`
  URL with a non-empty remainder, or `data:<mime>;base64,<payload>` with a
  non-empty mime type and payload (`validImageURL`/`splitDataURI`).
  Anything else, and any other part type (`input_audio`, `file`, ...), is
  rejected with a 400 naming the problem rather than silently dropped.
  Validating up front is what guarantees the later Anthropic conversion
  cannot lose an image. For OpenAI-compatible providers the gateway forwards
  the original body (model swapped) as raw passthrough, so image parts
  reach the provider as received.
- **Anthropic-format providers on the gateway** (`transformer.go`,
  `OpenAIContent.Anthropic()`): when the chain lands on anthropic, bedrock
  or vertex, `image_url` parts are converted to Anthropic image blocks in
  order (`anthropicImageBlock`): a base64 data URI becomes a `base64`
  source (media type and payload split out), an http(s) URL becomes a `url`
  source. Previously every part became a text block, so an image turned
  into an empty text block that those providers reject.
- **URLs are forwarded as received; NEXUS never fetches images.** An
  http(s) image URL is passed through to the provider (or converted to a
  `url` source) untouched. Whether a given provider can fetch URL images,
  and which image formats it accepts, is provider-dependent and not
  something NEXUS knows or checks; a provider that cannot handle it returns
  its own error, which is relayed unchanged. Fetching in a forwarder would
  add SSRF, size and timeout surface, so it was rejected, the same
  philosophy as "no chain filtering" below.
- **Privacy firewall leaves image bytes alone** (`firewall.go`):
  `redact` walks every JSON string and previously regex-scanned base64
  image payloads too. Random base64 can contain a secret-shaped substring
  (e.g. `AIza` followed by 20 URL-safe characters matches the Gemini key
  detector), which silently rewrote the image bytes into a placeholder and
  corrupted the image. It now skips (a) any string for which
  `isBase64DataURI` is true (OpenAI `image_url` data URIs; a cheap prefix
  test requiring `;base64,` within the first 128 bytes) and (b) the `data`
  member of an Anthropic `{"type":"base64", ...}` source. Text and URLs
  are still scanned.
- **Inspector omits image payloads** (`images.go` `elideImageData`,
  called from `handler.go`'s inspect capture): with `--inspect`, the
  marshaled request is stored capped at 64KB. Long (128+ char) base64
  `"data":"..."` values are replaced with `"data":"[image data omitted: N
  bytes]"` before the cap, so the budget is spent on text instead of a
  truncated base64 blob.
- **No chain filtering, by design:** `nexusImages` never reaches
  `RouteChain`, the sticky-provider logic, or the cooldown machinery — an
  image-bearing request walks the exact same provider chain a text-only
  request would. If the provider NEXUS picks can't actually handle the
  image, its own error response is relayed to Claude Code as-is, identical
  to how any other provider 4xx is already relayed today — an honest
  failure, not a NEXUS-side guess.
- **Semantic-cache exclusion** (`semantic.go`): `promptText` also
  returns `hasImages`, computed with the shared `contentHasImage` over the
  same `system`/message-`content` shape `collectText` already walks, so
  both the Anthropic (`image`) and OpenAI (`image_url`) shapes are caught.
  Both call sites — `HandleMessages` (`handler.go`) and
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
  router.StrategyDirect`). The helper `mappedModel` (`handler.go`)
  returns `requestedModel` verbatim when `directModel` is set, else falls
  back to `active.impl.MapModel(requestedModel)`. It is the fallback step
  of `upstreamModel(active, requestedModel, images)`, which is the single
  function that decides the model id sent to OpenAI-compatible providers
  and recorded as `model_used` (used by `callUpstreamOnce`,
  `callOpenAIPassthrough` in `gateway.go`, and the log/usage records in
  `openai.go` / `handler.go`; see "Vision / Image Content Support"). Its
  first check is `h.directModel`: when true it returns the requested id
  immediately, before any `vision_model` lookup — one branch bypasses
  `MapModel` and the `vision_model` override together, not two separate
  opt-outs.
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
  either, unlike `auto` (where `upstreamModel` lets `VisionCapable.
  VisionModel` replace the mapped model for an image-bearing request —
  see "Vision / Image Content Support" above). Under `direct` the
  requested model id always wins, image or not; the image itself is still
  forwarded.
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

## Model Catalog & Model-ID Routing

Read this before touching `GET /v1/models`, the background discovery loop, or
provider ordering in `RouteChain` / `PreferModel`. Spans
`internal/proxy/catalog.go` (`modelCatalog`, `catalogEntry`),
`internal/proxy/gateway.go` (`listedModels`, `HandleModels`,
`HandleChatCompletions`),
`internal/providers/models.go` (`ListModelDetails`, `ErrNoModelList`),
`internal/router/router.go` (`servingSet`, `preferModel`, `PreferModel`,
`SetProviderModels`, `ProviderServes`, `CascadeChain`),
`internal/proxy/handler.go` (`keyLocked`/`peekKey`, the `HandleMessages` chain
assembly, the `NewHandler` registration loop, and the cascade chain's
`PreferModel` call), and `internal/config/config.go` (`Catalog`).

The catalog is a cache, never a request-path dependency. That one sentence is
the whole design; the paragraphs below are the consequences of it.

- **The endpoint contract.** `GET /v1/models`
  (`internal/proxy/server.go` registers it for `GET` only) answers
  `{"object":"list","data":[{"id":…,"object":"model","owned_by":"nexus"}]}`
  in the catalog's stable order, which is provider registration order and then
  each provider's own list order. Entries may include the optional
  [`input_modalities` extension](docs/architecture.md#model-list-api).
  `modelListEntry` deliberately has **no
  `created` field** (D-10): NEXUS knows no honest creation time for a
  discovered id, and inventing one would be fabricated metadata. `owned_by` is
  `"nexus"` rather than the provider name because, after deduplication, an
  entry names a capability the proxy offers, not the provider that happens to
  serve it. The handler reads `modelCatalog.snapshot()` — a map scan under a
  read lock — and **never triggers discovery**: the endpoint is unauthenticated
  (no `deriveUser`, no key check), so a request-triggered fetch would let
  anyone on the LAN force keyed upstream calls (D-8). Warming is the
  background loop's job alone.
- **Why the list is discovered, not configured.** The proxy's provider set is
  fixed at startup — `NewHandler` registers it once from `config.Load` and
  nothing reloads it — so a configured list can only describe what the
  operator wrote at startup. Discovery is the only mechanism
  that reflects a provider whose catalog changes while NEXUS runs — the case
  the feature exists for is a local Ollama, where models appear and disappear
  with `ollama pull` and a restart-per-model would be absurd (D-2). The
  configured `models` are not discarded, though: they seed the catalog, so the
  list is useful before the first fetch lands (see the ladder below).
- **The `[catalog]` config keys.** `internal/config/config.go`'s `Catalog`
  struct carries `enabled` (default `true`) and `ttl_seconds` (default `300`).
  Both defaults are seeded in `config.Default()` and `Load` decodes on top of
  it, which is why an absent `[catalog]` table means *on* (the zero-config
  promise) while an explicit `enabled = false` really disables it — a plain
  bool decoded from nothing cannot express that difference. The off switch
  exists because polling every provider endpoint is network behaviour, and the
  project's rule is no telemetry or unsolicited outbound traffic without
  opt-in (D-11). A disabled catalog is still built and seeded, so the endpoint
  keeps a usable fallback; it simply never fetches (`catalogWarms`).
- **The fallback ladder, as it actually landed.** `Handler.listedModels`
  (`gateway.go`) is the one place the ladder lives, and it has two rungs:
  **the catalog snapshot — which already carries the seed until the first
  successful fetch replaces that list wholesale — then the three stub Claude
  ids** (`stubModels`, the pre-catalog behaviour, returned as a copy so a
  caller sorting it in place cannot corrupt the process-wide fallback).
  Recorded deviation from D-9: the plan words rungs two and three as a *global*
  gate — configured `models` first, and `providers.DefaultModels` only if *no*
  provider declares any — but `NewHandler` folds them **per provider** into the
  seed (`seedFor[name] = pc.Models` when non-empty, else
  `providers.DefaultModels(name)`), so a mixed config advertises declared ids
  *and* family defaults where the literal ladder would stop at rung two. The
  per-provider reading is the better one (it keeps every provider's fallback
  visible) and the routability invariant holds either way, because the seed is
  registered with the router. Second recorded deviation: the stub rung is
  **unroutable by construction** whenever a provider is registered but the
  catalog is empty, because an unknown family returns no `DefaultModels`; the
  routability guarantee therefore covers the discovered and seeded cases, not
  the last-resort stub.
- **Discovery mechanics.** `providers.ListModelDetails(p, apiKey)`
  (`internal/providers/models.go`) is one package-level function with a
  per-family recipe, **not** an interface method on each of the ~30 concrete
  provider types and **not** a method on the `overridden` config wrapper. That
  wrapper embeds the `Provider` interface, so extra methods are not promoted
  through it, and it is applied only conditionally (`generic.go`): a
  wrapper-based dispatch would silently miss unwrapped providers and an
  extra-interface assertion would silently miss wrapped ones. Hence one
  package-level function with an explicit `unwrapOverridden` step before the
  concrete-type switch. Which families can list, and how: the default
  OpenAI-compatible recipe is `GET <BaseURL()>/models` with a bearer token
  (`*Generic` included); `anthropic` uses `api.anthropic.com/v1/models` with
  `x-api-key` + `anthropic-version` because its `BaseURL()` stops at the host;
  `gemini` uses `v1beta/models?key=…` and strips the `models/` prefix because
  its `BaseURL()` points at the OpenAI shim; `ollama` uses the native
  `<baseURL>/api/tags`, which does not depend on the shim; `groq` needs an
  explicit `/v1/models` because its `BaseURL()` ends at `/openai`. `perplexity`
  and the enterprise families (`azure`, `vertex`, `bedrock`), which are
  Anthropic-native, have no listing endpoint and return `ErrNoModelList` — a
  distinguishable "cannot list" that means *keep the fallback forever*, as
  opposed to a failed listing that means *retry later*. The recipe is chosen by
  **concrete provider type**, never by `Provider.Name()`: `Name()` is the family
  keyword for built-ins — so `nexus add groq-us … --type groq` builds the
  built-in `*Groq` and registers as `groq` — and the operator's instance name
  for config-driven providers (`*Generic` and the enterprise types), which is
  exactly why it is not a reliable family key. A name switch would send a
  Bedrock provider called `prod-use1` down the generic recipe and a custom
  provider called `gemini` to Google with its key. A failed listing keeps the
  provider's last good list and backs off 30s doubling to a 5m cap
  (`nextCatalogBackoff`), mirroring the cooling-key reschedule philosophy;
  refreshes are single-flight per provider (`catalogEntry.inflight`), so a
  burst of picker requests cannot stampede an upstream (D-12). An **empty
  successful listing means "nothing learned", not
  "everything was withdrawn"**: it clears the failure state and restamps the
  entry but leaves both the models and the router registration untouched — a
  local Ollama with nothing pulled answers `/api/tags` with exactly that, and
  wiping the entry there would advertise seed ids the router no longer served.
- **Discovery must never perturb sticky key selection** (D-7).
  `activeProvider.keyLocked` (`handler.go`) takes an `adopt` flag;
  `pickKey` passes `true` and therefore **moves** the sticky cursor `a.rr`
  when it scans forward, while `peekKey` passes `false` and reports what
  `pickKey` would use without adopting it. Discovery goes through
  `discoveryKey()` → `peekKey()` only. The two cannot drift apart because the
  read path is shared, not duplicated. A provider whose whole pool is cooling
  is still listed on its primary `api_key`: a listing is a cheap GET that
  consumes no chat quota, and if that key is rejected the listing fails and
  backs off like any other failure.
- **Eligibility and selection.** Discovery updates the router's reverse index
  from model ID to registered providers under its lock. The routing strategy
  orders the eligible providers and the last successful provider for that model
  moves to the front if it is available. Both chat endpoints share this
  selection, including the Anthropic endpoint's cascade. If every provider for
  a known model is cooling, the proxy returns 503. After all providers have
  usable live listings, an unlisted exact ID returns 404. Incomplete discovery
  keeps the fallback chain, and non-direct strategies still accept Claude
  aliases and configured model mappings.
- **No `provider/model` prefix exists, and why** (D-1). The model id **is** the
  address: the client asks for an id, and the router resolves the destination
  from the candidates that serve it. A prefixed wire format was considered and
  **rejected by the human** — it would have changed what every client must
  send, and pinned the routing decision into the request string instead of
  leaving it to the candidates and their cooldown state. **Do not reintroduce
  it.** If a future session wants per-provider addressing, note that the
  existing escape hatches on the Anthropic endpoint are the `X-Nexus-Provider`
  header and config rules. The `direct` strategy forwards the requested ID
  verbatim to eligible OpenAI-compatible providers. Other strategies use the
  advertised ID by default and honor explicit model and vision overrides.
- **The duplicate-name rule.** `NewHandler` collapses entries that resolve to
  the **same registered name** — `impl.Name()` — to the **first** one, with
  a startup warning (`Duplicate provider family — skipping this entry`), and
  skips the rest entirely — `active`, `seedFor`, `providerOrder`, and the
  router all end up with exactly one. Which names collide depends on the
  provider: for built-ins `Name()` is the family keyword, so two entries under
  distinct config names (`groq-us` and `groq-eu`, both `type = "groq"`) do
  collide; for config-driven providers it is the operator's instance name, so
  two `type = "custom"` entries named `a` and `b`, or two Bedrock entries
  `prod-use1` and `prod-eu1`, register separately and never collapse. Partial
  collapse would be worse than the behaviour it replaces: the router keys
  providers by name, so a second same-named entry is invisible to
  `SetProviderModels`/`ProviderServes` (both stop at the first match) while
  `active` would keep only the last, and the two views would silently disagree
  about which key and which models are in play. This is a deliberate behaviour
  change: previously the router held two same-named entries while `active` kept
  the last one.
- **Three surfaces, and they can disagree.** `GET /v1/models` reads the
  discovery catalog, while the dashboard picker
  (`internal/dashboard/playground.go`, `handlePlaygroundModels`) reads each
  entry's `models` and falls back to `providers.DefaultModels`, and the
  `nexus models` CLI (`cmd/nexus/main.go`, `runModels`) prints the
  config-driven Claude-tier → provider-model mapping through `MapModel`. So a
  model the proxy accepts can be missing from the picker and the CLI, and a
  config-only id the dashboard shows may not be routable. That is deliberate
  scope control, not an oversight: the reconciliation is captured as the
  follow-up item T1 in `docs/plans/2026-11-17-model-catalog-and-id-routing.md`,
  which will file `docs/tickets/catalog-single-index.md` for one index shared by
  all three surfaces, with its home chosen so the proxy and dashboard can both
  reach it without an import cycle.

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

### Deploying (rpi)

Production runs on `rpi` (192.168.68.54, user `rpi`, key `~/.ssh/pipi`) —
**not** `opi` / `av-pc`:

- Checkout: `/home/rpi/sources/github.com/nexus-proxy` (compose project
  `nexus-proxy`, container `nexus`), ports 3000/2222, state in
  `~/.nexus/{config.toml,nexus.db}` on the host.
- Deploy = update the checkout, then `docker compose build && docker compose
  up -d`. The Node and Go-module layers are cached, so a source-only change
  rebuilds without network traffic.
- Verify: `curl -s localhost:3000/health`, `curl -s localhost:3000/v1/models`,
  and `curl -o /dev/null -w '%{http_code}\n' localhost:2222/`.
- **rpi cannot reach `opi`.** Its clone's `origin` points at
  `ubuntu@192.168.68.55`, but it holds no SSH key for it, so `git fetch
  origin` fails there. Ship commits as a bundle instead: `git bundle create
  /tmp/x.bundle <old-sha>..main vX.Y.Z`, `scp` it over, then `git fetch
  /tmp/x.bundle 'refs/heads/main:refs/remotes/bundle/main'
  'refs/tags/vX.Y.Z:refs/tags/vX.Y.Z'` and `git merge --ff-only bundle/main`.
- **rpi's login shell is fish.** `ssh rpi 'a && b'` dies with `command
  substitutions not allowed here`. Use `ssh rpi bash -s <<'REMOTE' … REMOTE`.
- `docker compose build` passes no build args, so the deployed binary reports
  `nexus dev (built unknown)`. CI passes `VERSION`/`BUILD_TIME`; wire them into
  the compose file if that matters.

### Local tooling reality

- `golangci-lint` is **not installed** and there is no `.golangci.yml`, so
  `make lint` cannot run — never report it as a passed check. Substitute `go
  vet ./...` plus `gofmt -l` on the files you touched.
- ~20 files carry **pre-existing** `gofmt` drift (`cascade_test.go`,
  `proxy/server.go`, `transformer.go`, `internal/config/config.go`, …). Leave
  them; reformatting them inside an unrelated change buries the real diff.
- `make test` is `go test ./... -v -race`. Filter it — `make test 2>&1 | grep
  -E '^(ok|FAIL|---)'` — instead of pulling the whole log into context.
- Any `internal/proxy` fixture that builds a handler (`NewHandler`, `New`)
  **must** call `scrubProviderEnv(t)` (see `env_test.go`). Without it the test
  picks up the developer's ambient provider env vars and makes **live API
  calls** with their real key. For catalog assertions, disable background
  warming (`[catalog] enabled = false`) and drive `discover(t, h)` yourself.

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
5. `internal/proxy/rules.go` — fallback chains (not `internal/router/rules.go`)
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
- **Images on the OpenAI gateway: strict inbound validation, one shared
  recursive detector, forward URLs as received** — `/v1/chat/completions`
  accepts `image_url` parts (http(s) URL or base64 data URI, validated at
  parse time; other part types still rejected) and converts them to
  Anthropic image blocks for anthropic/bedrock/vertex. A single recursive
  `contentHasImage` replaced the shallow per-site scans (the earlier
  "shallow by design" decision is reversed: it defeated the `vision_model`
  override for Claude Code's `tool_result` images). `upstreamModel` is the
  one place that picks the upstream/logged model id (vision override only
  for OpenAI-compatible providers; `direct` still wins unconditionally).
  The firewall never rewrites base64 image payloads, the inspector elides
  them, and NEXUS never fetches image URLs (provider-dependent support, the
  provider's own error is relayed). Still no vision-model catalog. See
  "Vision / Image Content Support" section above.
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
- **Model catalog: live discovery behind a TTL cache, and provider choice
  becomes model-id-aware** — `GET /v1/models` reports the deduplicated union
  of what the configured providers actually offer, discovered from each
  provider's own listing endpoint by a background loop and served from cache
  only (the endpoint is unauthenticated, so a request must never force keyed
  upstream calls). The list is discovered rather than configured because
  config is a startup snapshot with no reload path, which would leave a local
  Ollama's models invisible until restart; config `models` and
  `providers.DefaultModels` seed the catalog and form the fallback ladder down
  to the three stub Claude ids. The router's reverse index limits a known ID
  to its serving providers under every strategy. Both chat endpoints apply
  per-model stickiness within that set. The requested ID is the address, and
  `direct` forwards it verbatim. Discovery uses `peekKey` to read the current
  key without moving the sticky key cursor. See the
  "Model Catalog & Model-ID Routing" section above.

---

## What to NEVER Do

- Don't add a Python dependency
- Don't require Docker for basic usage
- No database server (SQLite only)
- Don't require a cloud account
- No telemetry without explicit opt-in
- Config always in `~/.nexus/` — never in the project dir
