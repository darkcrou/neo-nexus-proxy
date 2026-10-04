# Usage Measurement & Quota Tracking — Implementation Plan

Ticket: `usage-measurement` — Status: IN PROGRESS
Approved design: per-attempt immutable `usage_events`, query-time derived 5h windows
(provider-scoped, per-model breakdown), generic quota-header capture with typed
Anthropic transcription, NULL = "not reported". MCP dropped from scope (user
decision). Aborted streams get a marked `requests` row AND a partial usage event.
Probe requests recorded with `probe=1`.

## Process

1. Work through items in execution order, one at a time; run the relevant tests after each.
2. Core slices (U1–U5) are implemented in the main session — they touch the proxy hot
   path where accuracy is critical and context is already loaded. U7 (UI) is delegated
   to a general-purpose sub-agent with a precise spec once U6's API shapes are frozen.
3. Tests are written BEFORE the implementation of each slice (G3 tests-first).
4. After each slice: `go vet ./...` + `go test ./...` must pass before the slice is
   committed (atomic Conventional Commit per slice).
5. Final: Commit-Review-Fix cycle with the review sub-agent (G4), CHANGELOG + docs.
6. Status values: pending / in_progress / done / skipped. Update this file as slices complete.

## Dependency Analysis

- U1 (schema) blocks everything: all other items read/write `usage_events`.
- U2 (windows/aggregation) depends on U1 (same package, sequential).
- U3 (parsing) and U4 (recording hooks) touch the same files — implemented as one
  slice but tracked separately; U4 depends on U3's parsers and U1's recorder.
- U5 (abort handling) shares the relay files with U4 — grouped, done right after U4.
- U6 (dashboard API) depends on U2's query methods and U4 producing live events.
- U7 (UI panel) depends on U6's frozen API shapes. Backend-first, hard dependency.
- U8 (docs) last.

## Execution Order

| #  | ID | Title                                   | Severity | Status | Group        | Verification |
|----|----|-----------------------------------------|----------|--------|--------------|--------------|
| 1  | U1 | usage_events schema + record/fetch      | Critical | done    | Storage      | storage tests + migration test |
| 2  | U2 | window derivation + aggregations        | Critical | done    | Storage      | segmentation tests |
| 3  | U3 | presence-aware parsing + header capture | Critical | done   | Extraction   | parser unit tests |
| 4  | U4 | recording hooks in proxy hot path      | Critical | done   | Extraction   | handler/stream/gateway tests |
| 5  | U5 | aborted-stream handling + marked rows   | High     | done   | Grouped: Relays | abort tests |
| 6  | U6 | dashboard API endpoints                 | High     | done   | API          | dashboard tests |
| 7  | U7 | dashboard UI panel + rebuild embed      | Medium   | pending | UI (delegate) | build + embed + visual check |
| 8  | U8 | CHANGELOG + CLAUDE.md docs              | Medium   | pending | Docs         | review |

## Item Details

### U1 — usage_events schema + record/fetch
- **What:** New `usage_events` table (additive `CREATE TABLE IF NOT EXISTS` in
  `migrate()`), `UsageEvent` struct with pointer fields for token counts and
  quota fields (NULL = provider did not report), `RecordUsageEvent`,
  `GetUsageEvents(UsageFilter)`.
- **How:** Follow db.go/requests.go conventions verbatim: snake_case columns,
  UTC text `created_at` (`2006-01-02 15:04:05`), explicit column lists, no
  `SELECT *`, single-conn pool for concurrency, no mutexes. Indexes:
  `(provider, created_at)`, `created_at`, `model_used`.
- **Verify:** unit tests for NULL round-trip semantics; migration test that
  builds a pre-feature DB (old DDL only) and reopens via `New()`; concurrency
  test with `-race`.

### U2 — window derivation + aggregations
- **What:** `GetUsageWindows(provider, duration, limit)` — deterministic
  segmentation over events (see design below); `GetUsageTotals(filter, by)`;
  `GetProviderQuota()` (latest provider-reported quota per provider+dimension).
- **Segmentation rules (documented in code):** window starts at first event after
  previous window end. Termination precedence per adjacent event pair:
  (1) PROVIDER_RESET — a provider-reported 5h `quota_reset_at` (dimension "5h")
  observed inside the window passes before the next event → boundary at the
  reported reset; (2) RATE_LIMIT — a rate-limited event closes the window at its
  own timestamp (the 429 belongs to the window it terminates); (3) TIME_ELAPSED —
  next event after `start+duration` → boundary at `start+duration`. Trailing
  window closes by the same rules against `now`, else open. First window under a
  time horizon that may cut history → end reason UNKNOWN.
- **Derived totals per window:** fresh-in, cache-read, cache-write, out,
  reasoning, total_input = in+cache_read+cache_write, total_tokens,
  cache_hit_ratio = cache_read/total_input, request count, per-model breakdown,
  latest provider quota observation inside the window, rate-limited event count.
- **Verify:** table-driven tests: time-elapsed termination, rate-limit
  termination, provider-reset termination, open current window, per-model
  breakdown, UNKNOWN horizon case.

### U3 — presence-aware parsing + header capture
- **What:** Extend `internal/proxy/usage.go` with presence-aware raw usage
  (pointer fields; regex presence = reported) adding `reasoning_tokens`
  (`completion_tokens_details.reasoning_tokens`, stays included in Out — no
  double count); quota-header capture: prefix table (`anthropic-ratelimit-`,
  `x-ratelimit`, `x-ollama-`, `x-quota-`, `retry-after`, request-id family) →
  map → JSON; typed Anthropic unified transcription (`representative-claim` →
  dimension, matching utilization fraction + epoch reset — transcribed, never
  computed); retry-after seconds + reset timestamp parsing.
- **Verify:** fixture tests: Anthropic full/stream usage, GLM reasoning tokens,
  DeepSeek cache-hit, missing fields → NULLs, unified headers → normalized
  columns + raw JSON.

### U4 — recording hooks in proxy hot path
- **What:** `attemptMeta` struct created in `callUpstream` (key idx, target
  model, t0, status, headers snapshot), returned to chain walk, threaded into
  relays and `logResult`; `logResult` records the event for the relayed final
  attempt. Record meta-only events at every discard site: key-rotation 429s in
  `callUpstream`, failover `continue`s in both chain walks, transport errors,
  `probeKey` (probe=1), per-attempt events in `serveCascade` (bodies in hand —
  full usage extraction).
- **How:** mechanical signature additions; no behavior change to routing,
  cooldown, failover, cost, or `requests` logging. Cache hits get NO event
  (nothing consumed upstream).
- **Verify:** handler tests: 429 rotation → event with rate_limited + headers;
  failover → one event per attempt; probe → probe-flagged event; cascade →
  per-attempt events; happy path → one event with full usage; concurrency.

### U5 — aborted-stream handling (Grouped: Relays)
- **What:** In the three live stream relays (`relayAnthropicStream`,
  `relayOpenAIStream`, `relayOpenAIPassthroughStream`): detect client
  disconnect/context cancellation + upstream read errors; record partial usage
  with `usage_partial=1` + error text on the event, and a `requests` row with
  the existing (never-used) `Error` column = "aborted: client disconnected"
  (or upstream error variant). Add the missing context checks to the two OpenAI
  relays so aborted client connections stop consuming upstream quota.
- **Verify:** tests: client-abort mid-stream → partial event + marked requests
  row; upstream read error mid-stream → partial event; openAI-stream abort
  stops relaying.
- **Status:** done — `logAbortedStream` helper (handler.go) marks both records;
  `abortClientGone`/`abortUpstreamErr` constants; capture-before-write in the
  two byte-copy relays so aborted chunks are still counted; `relayOpenAIStream`
  gained an `r` param + error-returning `send` + scanner.Err()/context checks;
  `relayOpenAIPassthroughStream` gained an `r` param + EOF/error split + context
  check; 9 tests in usage_events_test.go (3 abort modes × 3 relays, plus
  completed-stream not marked).

### U6 — dashboard API endpoints
- **What:** `GET /api/usage/windows?provider=&dimension=5h&limit=` (current +
  historical), `GET /api/usage/events?provider=&model=&limit=`,
  `GET /api/usage/totals?by=provider|model&period=`,
  `GET /api/usage/quota`. Follow server.go conventions: `?period=` enum,
  snake_case JSON, plural-key envelopes, empty shapes when db==nil.
- **Verify:** dashboard tests seeding events via `RecordUsageEvent` and
  asserting JSON shapes (full-router style + handler-direct style).
- **Status:** done — `internal/dashboard/usage.go` (4 handlers, empty shapes on
  nil db, plural envelopes), routes registered in server.go; providerless
  `/windows` derives the provider list from usage totals; 5 tests in
  dashboard/usage_test.go.

### U7 — dashboard UI panel (delegate)
- **What:** "Usage windows" panel in App.svelte + stores: current window per
  provider (token totals, cache-hit ratio, provider-reported utilization where
  available, window end/reset), recent windows table with end reasons,
  drill-down to per-event history via `/api/usage/events`. Design tokens
  strictly from the existing palette. `make build-web && make embed`.
- **Verify:** web build green; visual check via vision agent; API shapes frozen
  from U6.

### U8 — docs
- **What:** CHANGELOG Unreleased bullets; CLAUDE.md section documenting the
  subsystem, NULL semantics, window rules, provider limitations (Z.ai credits
  not in API responses; Ollama Cloud no quota headers — ollama/ollama#15663).
- **Verify:** review pass.

## Completion Log

| Date | Item | Notes |
|------|------|-------|