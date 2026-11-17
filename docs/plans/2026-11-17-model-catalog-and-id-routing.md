# Model catalog endpoint and model-id-aware provider selection

## Process

### Approved task context

- **Goal and scope.** Make `GET /v1/models` on the proxy port report the models that are actually reachable through NEXUS, and make provider selection honour the requested model id so that every advertised id can really be served. Two coupled changes:
  1. `/v1/models` returns the deduplicated union of models discovered live from each configured provider, cached with a TTL.
  2. `RouteChain` prefers providers that serve the requested model id, for every routing strategy, including `direct`.

  Out of scope: `provider/model` prefix routing (explicitly rejected by the human), virtual/synthetic model ids, a hardcoded vision- or capability-model catalog, any change to the `direct` strategy's model-id passthrough promise, and any change to the sticky provider/key cooldown machinery.

- **Success criteria.**
  - `GET /v1/models` returns an OpenAI-shaped list whose entries are the deduplicated union of what the configured providers report, in a stable order, served from cache without blocking on upstream calls.
  - For every id returned by `/v1/models`, a request naming that id places at least one provider that serves it at the head of the chain, as reported by the router's locked accessor `Router.ProviderServes(chain[0].Name, id)`, under every strategy including `direct`.
  - Provider listing failures never break the endpoint: the list degrades through the documented fallback ladder.
  - Routing behaviour is unchanged when the requested id is served by no provider, and when every provider serves it.
  - The catalog-unification follow-up (the dashboard's `/api/playground/models` and the `nexus models` CLI sharing one index with `/v1/models`) is captured as an intake ticket under `docs/tickets/` (item T1).
  - `make test` (which runs `go test ./... -v -race`) and `make lint` (golangci-lint) pass.

- **Context and decisions.** The investigation and the option comparison happened in the session that produced this plan; the decisions below are the durable record.
  - **D-1 (human decision).** No `provider/model` prefix. The model id is the address; the router resolves the destination from the candidates that serve it. This keeps the wire format unchanged for every client and leaves `direct`'s "forward the requested model id verbatim" promise intact, because only *provider choice* becomes id-aware.
  - **D-2 (human decision).** The list comes from live discovery against each provider, cached with a TTL, rather than from config alone. Config is a startup snapshot with no reload path, so discovery is also the only way a dynamically changing provider (notably a local Ollama) can be reflected without a restart.
  - **D-3 (human decision).** Model-id-aware provider selection is part of this work. Without it the endpoint advertises ids the router cannot reach: `isRetryableStatus` (`internal/proxy/handler.go:1432-1442`) does not include 404, so a provider that does not serve the requested id returns 404 straight to the client, and `setSticky` is written only on success, so the failure never teaches the router anything.
  - **D-4 (technical decision).** Discovery is a package-level function in `internal/providers`, `ListModels(p Provider, apiKey string) ([]string, error)`, with a switch on the provider family. It is **not** a new interface method implemented on every concrete provider type, and **not** a method on the `overridden` wrapper: that wrapper is applied conditionally (`internal/providers/generic.go:97`), so a provider with no config overrides is returned unwrapped and a type assertion would silently fail. Roughly 30 concrete provider structs exist, and one function keeps the per-family URL and header rules in one place.
  - **D-5 (technical decision).** The index is fed into the existing `router.Provider.Models` map, which is `map[string]string` (requested id to provider id) and is **currently never populated in production**; only `manualChain` reads it, and only under `StrategyManual`. `RouteChain` gains a stable partition so all strategies honour it. This follows the precedent recorded in `CLAUDE.md` under "Provider & Key Cooldown": reuse existing plumbing by feeding it a better signal instead of building a parallel mechanism. It also revives `manualChain`, which is dead in production today.
  - **D-6 (technical decision).** Reordering precedence is: strategy chain, then `stickyReorder`, then the stable model partition. Sticky therefore decides among providers that serve the id, while providers that do not serve it always sort last. A stable partition preserves the order produced by the earlier steps inside each group.
  - **D-7 (technical decision).** Discovery must not perturb sticky key selection. `activeProvider.pickKey` mutates `a.rr` when it scans forward (`internal/proxy/handler.go:81`), so discovery uses a new non-mutating accessor instead.
  - **D-8 (technical decision).** `/v1/models` never blocks on discovery. It serves a cached snapshot; warming happens in a background loop. The endpoint is unauthenticated today (no `deriveUser`, no key check, `internal/proxy/gateway.go:165-175`), so request-triggered discovery would let anyone on the LAN force keyed upstream calls.
  - **D-9 (technical decision).** Fallback ladder when nothing is discovered: live discovery, then config `[[providers]] models`, then `providers.DefaultModels(name)` (`internal/providers/provider.go:229`), then the three Claude ids the stub returns today, so a client never sees an empty list.
  - **D-10 (technical decision).** No fabricated metadata. Omit `created` rather than invent a timestamp; report `owned_by: "nexus"` because with dedupe the entry names a capability, not a destination.
  - **D-11 (technical decision).** A new `[catalog]` config section carries `enabled` (default true) and `ttl_seconds` (default 300). Background polling of every provider endpoint is a network behaviour the operator must be able to switch off, consistent with the project's "no telemetry without opt-in" rule.
  - **D-12 (technical decision).** Failure policy: a failed listing keeps the provider's last good list, and the provider backs off exponentially from 30s to a 5m cap, mirroring the reschedule philosophy already used for cooling keys. Refreshes are single-flight per provider so a burst of dashboard or picker requests cannot stampede an upstream.

- **Human authorizations and exceptions.** The human approved the goal, the three decisions D-1 to D-3, and asked for this persistent plan. No approval gates were waived: approval of this document is required before execution, and the final completion approval is required at the end.

### Work units and checkpoints

Each numbered item below is one coherent unit: tests first, implementation, verification, one commit, then independent review of that commit range before the item is marked `done`.

| Unit | Verification command | Expected result | Commit boundary |
| --- | --- | --- | --- |
| R1 | `go test ./internal/router/ -v -race` | New partition tests pass; all existing router tests pass | `feat(router): order providers by whether they serve the requested model` |
| C1 | `go test ./internal/providers/ -v -race` | Parser tests pass for every family and for the unsupported case | `feat(providers): list models from a provider endpoint` |
| C2 | `go test ./internal/proxy/ -run TestCatalog -v -race` | TTL, backoff, single-flight, and snapshot ordering tests pass | `feat(proxy): TTL-cached model catalog fed by live discovery` |
| P1 | `go test ./internal/proxy/ -v -race` plus `make test` | Endpoint, fallback, and routability-invariant tests pass; full suite green | `feat(proxy): serve discovered models from /v1/models and route by model id` |
| D1 | `make lint` | No new lint findings | `docs: document the model catalog and id-aware routing` |
| T1 | `scripts/ticket show catalog-single-index` | Ticket prints with the five frontmatter keys in order and a body naming the three surfaces | `docs: capture the single-catalog-index follow-up as a ticket` |

Review checkpoints: after each unit's commit, the main agent hands the commit range plus the approved goal, scope, success criteria, and verification evidence to the `review` sub-agent. Blocking findings are corrected by a delegated implementation sub-agent, verified, committed, and reviewed again.

### Execution procedure

Execution begins only after the human approves this plan. The executor may be the agent that created the plan or an agent in a fresh session. Read the approved task context and follow the Process and assigned work; do not reconstruct instructions from the planning conversation or change the approved approach without a human decision.

1. Work through the execution order one item at a time. Items are not grouped; execute them in the order given.
2. **Delegate each item to an implementation sub-agent.** Provide the plan location, the assigned item ID, and the full item details. Before starting, the sub-agent reads the shared Process, approved task context, status definitions, assigned details, and relevant Completion Log entries. It does not need unrelated item details. The main agent coordinates rather than performing implementation itself.
3. **The executor assesses the item before implementation.** It reads the current code and identifies the gap within the approved approach.
   - **Resolve technical choices independently.** If several implementation approaches are reasonable, choose the one that best fits the existing code and the user's instructions. Having multiple options is not itself a reason to stop.
   - **Ask the user about unresolved UI/UX preferences or domain logic.** If existing instructions already answer the question, proceed.
   - **When a user decision is needed, pause the affected item.** The sub-agent marks it `blocked_needs_user_input` and reports the question, options, and relevant trade-offs to the main agent, which obtains the user's decision and passes it back.
   - **Investigate technical blockers independently.** A missing field or unclear implementation is not automatically a user decision. Attempt to resolve it within the authorized scope. If it cannot be resolved, mark the item `blocked` and report the concrete blocker.
4. **The executing sub-agent owns its assigned item's progress record.** It marks the item `in_progress` before starting work, `blocked_needs_user_input` when a user decision is needed, and `blocked` when a technical blocker cannot be resolved. Keep the item `in_progress` through verification, committing, and independent review. Record verification evidence, commit identifiers, review outcomes and correction rounds, blockers, and reasons for skipping in the Completion Log.
5. The executor must read the relevant current code before modifying it.
6. **The executor writes tests first, then implements the approved change.** The tests express the item's agreed target state in human-readable business language: the behavior and its observable outcome, not the internal structure, using the project's existing test framework. They define what `done` means for the item, and implementation code follows the contract they establish. When a test cannot be written first, record why and obtain explicit human approval before skipping the requirement.
7. The executor uses the repository's documented verification commands for the affected components: `make test` (`go test ./... -v -race`) and `make lint` (`golangci-lint run ./...`).
8. **Visual verification is not required for this work.** No web UI is touched; the dashboard reads its own `/api/playground/models` endpoint and is deliberately left unchanged. If a later decision surfaces a UI change, record it as a new item rather than expanding an item below.
9. **Commit before independent review.** After completing and verifying a unit, commit its changes using the commit boundary named in the table above. The main agent hands the committed change range to an independent review sub-agent, providing the agreed goal, scope, success criteria, and verification evidence. The reviewer evaluates the actual committed changes against that outcome and checks for defects and regressions introduced by the work. Blocking findings must explain their connection to the agreed outcome or an introduced regression. Unrelated pre-existing issues and optional improvements do not block review approval or authorize extra implementation.
   - If review identifies blockers, the main agent delegates their correction to an implementation sub-agent. Verify and commit the fixes, then request another independent review. Preserve unresolved findings across rounds. Repeat until no blocking findings remain.
   - If a necessary correction would change the approved scope or approach, obtain a human decision before that dependent work proceeds.
   - Mark an item `done` only after verification, committing, and independent review have passed. Optional recommendations do not extend the cycle. If a required checkpoint cannot be performed, report the concrete blocker rather than silently omitting it.
10. **The main agent coordinates execution.** The implementing and reviewing sub-agents send brief reports containing item IDs, results, and any question or blocker requiring attention. The implementing sub-agent records review outcomes for its assigned item. The main agent does not duplicate completion entries or reread implementation details merely to record progress.
11. **Do not batch items.** Each item is executed and reviewed on its own.
12. The executor implements prerequisites before dependent work. Mark required prerequisites in the plan.
13. **A blocked item does not pause independent work.** Mark the item `blocked_needs_user_input` or `blocked`, and record the decision or prerequisite needed to resume it. Continue with the next item in the execution order that has no unresolved hard dependencies. Leave dependent items pending and identify the blocking prerequisite. Once the blocker is resolved, return to the earliest item that is ready to execute.
14. **Progress ownership follows reassignment.** A new owner reads the Process section, assigned item details, status, and existing log before continuing. Preserve earlier entries and add updates under the same item ID.
15. **Skipping an item does not automatically satisfy its dependencies.** Before starting dependent work, the executor checks whether the required result already exists or the prerequisite no longer applies. Record why the dependency is satisfied or no longer needed. Otherwise, leave dependent work pending and record the unmet prerequisite.
16. **Assess overall completion against the agreed success criteria.** Perform required verification across units. Further implementation corrections are delegated, verified, committed, and independently reviewed through the same cycle. Do not repeat an equivalent completed review of unchanged committed work. Present the delivered result, verification evidence, remaining limitations, and optional recommendations separately. Combine the final approval request with the choice about saving a completion report under `docs/reports/`. Obtain explicit human approval before treating the workflow as complete.

## Dependency Analysis

- **R1 blocks C2 and P1.** C2 pushes discovered ids into the router, so the `SetProviderModels` setter and the partition in `RouteChain` must exist first.
- **C1 blocks C2.** C2 consumes `providers.ListModels`.
- **C2 and R1 block P1.** P1 wires the catalog into the handler and rewrites the endpoint.
- **P1 blocks D1.** Documentation describes the delivered behaviour.
- **P1 blocks T1.** The ticket describes the state this plan leaves behind and links to this plan, so it is written after P1 lands.
- **D1 and T1 are independent of each other.** Both depend only on P1 and touch different files (`CLAUDE.md`, `CHANGELOG.md`, `docs/architecture.md` versus a new file under `docs/tickets/`). They are ordered D1 then T1 so the delivered behaviour is documented before the follow-up is filed against it.
- **C1 and R1 are independent of each other.** They touch different packages (`internal/providers` and `internal/router`), share no files, and can be verified separately. They are ordered R1 before C1 only because R1 is the smaller unit and unblocks the critical path sooner.
- **No grouping.** No two items share implementation touchpoints in a way that justifies a single sub-agent pass.
- **Shared code paths to watch.** Both C2 and P1 touch `internal/proxy/handler.go` (C2 adds the catalog field and its lifecycle, P1 changes `NewHandler` wiring and `HandleModels`). They stay separate items because C2's boundary is the cache and its lifecycle, while P1's boundary is the endpoint contract and the fallback ladder; P1 rebases on C2's committed state.

## Execution Order

**Allowed statuses:** `pending` — not started; `in_progress` — underway, including verification, committing, and independent review; `blocked_needs_user_input` — waiting for a user decision; `blocked` — unable to proceed because of an unresolved technical blocker; `done` — completed and verification, committing, and independent review passed; `skipped` — intentionally not completed, with the reason recorded in the Completion Log.

| # | ID | Title | Severity | Status | Group | Prerequisites | Verification |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | R1 | Model-id-aware provider partition in the router | High | done | -- | None | `go test ./internal/router/ -v -race` |
| 2 | C1 | Per-family model listing in the providers package | High | done | -- | None | `go test ./internal/providers/ -v -race` |
| 3 | C2 | TTL-cached model catalog with background discovery | High | done | -- | R1, C1 | `go test ./internal/proxy/ -run TestCatalog -v -race` |
| 4 | P1 | Serve discovered models from `/v1/models` and route by id | High | done | -- | C2, R1 | `make test` |
| 5 | D1 | Documentation and changelog | Medium | pending | -- | P1 | `make lint` |
| 6 | T1 | Ticket for the single-catalog-index follow-up | Low | pending | -- | P1 | `scripts/ticket show catalog-single-index` |

## Item Details

### R1: Model-id-aware provider partition in the router

- **Current state:** `router.Provider.Models` is `map[string]string` (`internal/router/router.go:49-59`) and is never populated in production: the only production registration is `handler.go:270-277`, which omits it. The only reader is `manualChain` (`router.go:284-294`), reached only under `StrategyManual`, so in production `manualChain` always falls through to `rest` and behaves like an unordered healthy list. `RouteChain` (`router.go:184-197`) passes `requestedModel` only to `manualChain`; `autoChain`, `byCost`, and `byTierSpeed` ignore it.
- **Expected state:** Providers that serve the requested model id are ordered ahead of providers that do not, under every strategy, without changing the relative order inside either group. `manualChain` keeps its current observable behaviour, now produced by the shared helper instead of a duplicated loop.
- **What:** Add `func (r *Router) SetProviderModels(name string, ids []string)`, which replaces that provider's `Models` map under the existing `r.mu`, mapping each id to itself. Add a stable partition step applied by `RouteChain` after the strategy switch. Reimplement `manualChain` on top of the shared helper, or delete it and route `StrategyManual` through the partition, whichever keeps the diff smaller and the behaviour identical.
- **Why:** This is the mechanism that makes an advertised id reachable (D-3, D-5). Without it, a request for a discovered id goes to whatever provider the complexity chain happened to put first; that provider returns 404, `isRetryableStatus` (`handler.go:1432-1442`) does not treat 404 as retryable, and `setSticky` only records successes, so the id never becomes routable.
- **How:**
  - Keep the strategy functions pure. Compute the chain exactly as today, then apply the partition to the result.
  - The partition must be **stable**: iterate the chain once, appending to a `match` slice or a `rest` slice, then concatenate. Do not sort; sorting would destroy the sticky order applied later.
  - `SetProviderModels` must tolerate a provider name that is not registered (log at debug and return) because discovery and registration can race at startup.
  - `Models` values are the provider model id, so set `Models[id] = id`. Discovery results are already provider ids, not Claude ids.
  - Expose a locked read accessor, `func (r *Router) ProviderServes(name, id string) bool`, because `Models` is an exported field on an exported struct and therefore reachable without the lock from any `*Provider` returned by `RouteChain` or `Providers()`. Note on the field that external readers must use the accessor. This is what keeps P1's routability invariant race-free against C2's discovery loop.
  - Do not add a field to `Provider` for "discovered" versus "configured" ids unless a later item needs the distinction; keep the map homogeneous.
- **Prerequisites:** None.
- **Verify:** New tests in `internal/router/router_test.go` (or a new `partition_test.go`) covering: under `StrategyAuto`, a provider that serves the id moves ahead of one that does not; under `StrategyDirect`, `StrategyCheapest`, and `StrategyFastest`, the same; the partition is a no-op when no provider serves the id and when all do; relative order within each group is preserved; `StrategyManual` still puts serving providers first; `SetProviderModels` on an unknown name does not panic. Then `go test ./internal/router/ -v -race` and the full `make test`.

### C1: Per-family model listing in the providers package

- **Current state:** Nothing in the repo parses a model-listing response. Every `/models` request is status-code-only: `bearerHealthCheck` (`internal/providers/provider.go:300-318`) closes the body unread, and the six providers with custom health checks do the same. The only model lists that exist are hardcoded: `providers.DefaultModels` (`provider.go:229`) and the `HandleModels` stub (`internal/proxy/gateway.go:166-175`).
- **Expected state:** A single function returns the model ids a provider currently offers, or a distinguishable "this provider cannot list" result, using the endpoint and authentication each family actually needs.
- **What:** New file `internal/providers/models.go` exporting `func ListModels(p Provider, apiKey string) ([]string, error)` plus the response parsers. Families:
  - OpenAI-compatible (the default): `GET <BaseURL()>/models` with `Authorization: Bearer <key>`, parse `{"data":[{"id":...}]}`.
  - `anthropic`: `GET https://api.anthropic.com/v1/models` with `x-api-key` and `anthropic-version: 2023-06-01`, parse `{"data":[{"id":...}]}`.
  - `gemini`: `GET https://generativelanguage.googleapis.com/v1beta/models?key=<key>`, parse `{"models":[{"name":"models/<id>"}]}` and strip the `models/` prefix.
  - `ollama`: `GET <BaseURL() minus the trailing /v1>/api/tags`, parse `{"models":[{"name":...}]}`. The native endpoint is authoritative and does not depend on the OpenAI shim.
  - Unsupported: `perplexity`, and the enterprise providers (`azure`, `vertex`, `bedrock`), which are Anthropic-native and have no OpenAI-style listing.
- **Why:** C2 needs a per-provider source of truth (D-2, D-4). Putting the family rules in one function keeps the ~30 concrete provider types untouched and sidesteps the conditional `overridden` wrapper, whose absence would silently defeat an interface-based approach.
- **How:**
  - Derive URLs from `p.BaseURL()` rather than hardcoding hosts, except where the provider's `BaseURL()` does not include the API prefix (`anthropic`, `gemini`), which is the same split the existing custom `HealthCheck` implementations already encode.
  - Use a dedicated `http.Client` with a 5s timeout, mirroring `bearerHealthCheck`.
  - Cap the parsed list (for example 2000 ids) and reject non-2xx with an error that names the status, so C2 can log it.
  - Define an exported sentinel `ErrNoModelList` for the unsupported case so C2 can distinguish "cannot list, keep the fallback" from "listing failed, back off".
  - Trim and drop empty ids; preserve the provider's own ordering.
- **Prerequisites:** None.
- **Verify:** New `internal/providers/models_test.go` with `httptest` servers returning one fixture per family, asserting the parsed ids and the `models/` prefix stripping; an unsupported provider returning `ErrNoModelList`; a 500 and a malformed body both returning an error; and a request-header assertion proving the bearer key is sent for the OpenAI-compatible case and `x-api-key` plus `anthropic-version` for Anthropic. Then `go test ./internal/providers/ -v -race`.

### C2: TTL-cached model catalog with background discovery

- **Current state:** No cache, no background discovery, no index of provider models anywhere in the proxy. The handler's only background work is `keyProbeLoop`, started at `internal/proxy/handler.go:361` with `go h.keyProbeLoop(h.stopHealth)` and stopped by `Close()` (`handler.go:367-373`), which closes `stopHealth` and nils it without a mutex. `activeProvider.pickKey` (`handler.go:66-86`) mutates `a.rr` when scanning forward; `hasAvailableKey` (`handler.go:91-`) is the non-mutating counterpart.
- **Expected state:** A catalog that, for each registered provider, holds the last known model list with a fetch timestamp and a backoff state, refreshes in the background on a TTL, exposes a lock-safe snapshot, and reports which providers serve a given id. It never blocks a request path and never perturbs sticky key selection.
- **What:** New file `internal/proxy/catalog.go` with an unexported `modelCatalog` type and its lifecycle wiring in `Handler`.
- **How:**
  - Struct: `mu sync.RWMutex`, `entries map[string]*catalogEntry`, `rt *router.Router`, `providers map[string]*activeProvider`, `ttl time.Duration`, `now func() time.Time` (injected for tests). `catalogEntry` holds `models []string`, `fetchedAt time.Time`, `err string`, `backoff time.Duration`, `inflight bool`.
  - `refreshAll(ctx)`: for each provider, skip when `inflight`, skip when the entry is fresher than `ttl`, otherwise mark inflight, fetch, then update. On success clear the error and reset the backoff; on failure keep the previous `models`, set the error, and grow the backoff 30s to 5m. On `ErrNoModelList`, record it once and never schedule that provider again.
  - Single-flight is per provider: the `inflight` flag under `mu` is enough because only the background loop fetches. If a synchronous on-demand fetch is added later, it must respect the same flag.
  - Key selection: add a non-mutating `peekKey() (string, bool)` to `activeProvider` that mirrors `pickKey`'s read path without assigning `a.rr`, and use it for listing. Never call `pickKey` from discovery (D-7).
  - Push results into the router with `rt.SetProviderModels(name, ids)` after each successful fetch, so R1's partition sees them.
  - `snapshot() []string`: dedupe by id, order by the provider registration order and then by each provider's own list order, so the output is stable across calls.
  - Lifecycle: construct in `NewHandler`, seed from config `models` and `providers.DefaultModels(name)` so the catalog is useful before the first fetch completes, start the warm loop on the existing `h.stopHealth` channel (a second goroutine reading the same channel is fine because `Close` closes it once), and honour the `[catalog]` config section (D-11): when `enabled` is false, keep the seed-only behaviour and start no loop.
  - Keep the warm loop's tick well below the TTL (for example TTL/5, minimum 10s) so refreshes are spread rather than synchronized, and log at debug level on success and warn on failure.
- **Why:** This is the cached, non-blocking source of truth for both the endpoint and the router index (D-2, D-8, D-12).
- **Prerequisites:** R1 (for `SetProviderModels`), C1 (for `ListModels`).
- **Verify:** New `internal/proxy/catalog_test.go`, all names prefixed `TestCatalog`, using fake `httptest` upstreams and the injected clock: a first fetch populates the snapshot; a second snapshot inside the TTL does not hit the upstream (assert the request counter); advancing the clock past the TTL refetches; a failing provider keeps its last good list and the next refresh is delayed by the backoff; a recovering provider clears the error and resets the backoff; two providers offering the same id appear once in the snapshot; snapshot order is stable across calls; an unsupported provider is not probed twice; discovery never changes the sticky key selection (assert `a.rr` before and after); and `go test -race` reports no data race with concurrent `snapshot()` calls during a refresh. Then `go test ./internal/proxy/ -run TestCatalog -v -race`.

### P1: Serve discovered models from `/v1/models` and route by id

- **Current state:** `HandleModels` (`internal/proxy/gateway.go:165-175`) returns a hardcoded three-item list (`claude-opus-4-5`, `claude-sonnet-4-6`, `claude-haiku-4-5`) with `owned_by: "nexus"`, and builds an unused `seen` map. It performs no authentication, no provider contact, and no logging. The route is registered at `internal/proxy/server.go:70` for GET only. `TestGateway_Models` (`gateway_test.go:369`) asserts only that `object` is `list` and `data` is non-empty; `routes_test.go:75-80` asserts a 200 with `"object":"list"`.
- **Expected state:** The endpoint returns the deduplicated union of discovered models in a stable order, degrading through the documented fallback ladder, and every id it returns is routable to a provider that serves it.
- **What:** Rewrite `HandleModels` to read the catalog snapshot, apply the fallback ladder, and drop the dead `seen` map. Wire the catalog into `NewHandler` so the router receives the index, and make the requested model id influence the chain at both call sites.
- **How:**
  - Response shape: `{"object":"list","data":[{"id":<id>,"object":"model","owned_by":"nexus"}]}`. Omit `created` (D-10). Preserve the snapshot's stable order.
  - Fallback ladder (D-9): discovered ids; if empty, config `[[providers]] models`; if still empty, `providers.DefaultModels(name)` per provider; if still empty, the three Claude ids the stub returns today. Keep this ladder in one helper so the endpoint and any future caller share it.
  - Chain assembly order (D-6): `RouteChain` output, then `stickyReorder`, then the router's stable partition. Since R1 puts the partition inside `RouteChain`, verify that composition still yields the intended precedence at both call sites (`handler.go:679` and `gateway.go:109`); if the partition runs before `stickyReorder`, add the partition as an explicit post-step at those call sites instead, and record the change in the Completion Log.
  - Do not add authentication to the endpoint; do not let it trigger discovery (D-8).
  - Leave the dashboard's `/api/playground/models` (`internal/dashboard/playground.go:30-53`) and the `nexus models` CLI (`cmd/nexus/main.go:374`) unchanged in this item. They read config, not the catalog, and reconciling them is separate work: record the inconsistency in the Completion Log, and capture it as item T1 rather than expanding scope.
- **Why:** This is the user-visible half of the feature and the point where the list stops lying. The routability invariant is what makes the promise real.
- **Prerequisites:** C2 (catalog), R1 (partition).
- **Verify:** New tests in `internal/proxy/models_test.go` (plus updates to the two existing tests above):
  - With two fake providers exposing different `/models` fixtures, the endpoint returns the union with no duplicates.
  - A duplicate id across two providers appears exactly once.
  - When every provider listing fails and config declares no models, the ladder still yields a non-empty list.
  - The response carries no `created` field and every entry has `object: "model"`.
  - **Routability invariant:** for every id in the response, `RouteChain(id, complexity)` for each of `simple`, `standard`, `complex`, and `critical` returns a chain whose head the router reports as serving that id, checked with `Router.ProviderServes(chain[0].Name, id)` rather than by reading `Provider.Models` directly, so the assertion stays race-free while C2's discovery loop writes. This is the regression guard that keeps the promise keepable.
  - An end-to-end request naming a discovered id reaches the provider that serves it (fake upstream returns 200 only for that id) under `direct` as well as `auto`.
  - `GET` remains the only accepted method; a `POST` still falls through to the catch-all 404.
  Then `make test` and `make lint`.

### D1: Documentation and changelog

- **Current state:** `/v1/models` is documented nowhere: `README.md` lists only `/v1/messages` and `/v1/chat/completions` (line 77), `docs/architecture.md`'s endpoint diagram omits it, and `CLAUDE.md` has no section for it. `CHANGELOG.md` has an `## Unreleased` section.
- **Expected state:** The endpoint's behaviour, the discovery cache, the fallback ladder, and the id-aware partition are documented where a future contributor will look, and the user-facing change is in the changelog.
- **What:** Add a `CLAUDE.md` section in the style of the existing "Provider & Key Cooldown" and "Vision / Image Content Support" sections, covering: the endpoint contract; why the list is derived from live discovery and TTL-cached; the `[catalog]` config keys; the fallback ladder; that discovery must never perturb sticky key selection; that the partition runs after `stickyReorder`; that no `provider/model` prefix exists and why; and that `direct`'s model-id passthrough promise is unchanged. Add a `Critical Decisions` bullet. Add the endpoint row to the proxy table in `CLAUDE.md` and to the endpoint diagram in `docs/architecture.md`. Add a bullet under `## Unreleased` in `CHANGELOG.md` describing the user-facing change.
- **Why:** The repo's convention is that consequential mechanisms are recorded in `CLAUDE.md` before anyone touches the code again, including the reasons a rejected design was rejected.
- **How:** Follow the existing section style: prose paragraphs, bold lead-ins, file and line references, and an explicit "do not redo this" note for the rejected prefix design, mirroring the vision-catalog paragraph.
- **Prerequisites:** P1.
- **Verify:** `make lint` clean; a read-through confirming the documented chain order matches the code; the `CHANGELOG.md` bullet reads as a user-facing change rather than an implementation note.

### T1: Ticket for the single-catalog-index follow-up

- **Current state:** `docs/tickets/` holds two hand-written files (`gateway-image-support.md`, `usage-measurement.md`) that predate the ticket format and carry no frontmatter, which is why the ticketing CLI reports no tickets for this repository. Nothing records the catalog inconsistency this plan deliberately leaves behind: after P1, `GET /v1/models` reads the discovery catalog while `GET /api/playground/models` (`internal/dashboard/playground.go:30-53`, config `models` else `providers.DefaultModels`) and the `nexus models` CLI (`cmd/nexus/main.go:373-394`, config plus the `MapModel` Claude-tier mapping) still derive their lists from config.
- **Expected state:** A valid intake ticket exists for unifying the three model-listing surfaces, written for a contributor who was not in this session, and linked from this plan.
- **What:** Create `docs/tickets/catalog-single-index.md` as the intake record for that follow-up. Do not implement the unification.
- **Why:** Three surfaces will disagree once P1 lands. A model the proxy accepts may be missing from the dashboard picker and the CLI, and a config-only fallback shown by the dashboard may not be routable. Leaving that undocumented guarantees a future contributor rediscovers it from scratch.
- **How:**
  - Write the file by hand. Hand editing is supported by the ticket convention; the CLI is a convenience, not a gatekeeper.
  - Frontmatter, exactly these five keys in this order and nothing else:

    ```
    ---
    status: open
    priority: medium
    created: <the date the ticket is written>
    started: null
    completed: null
    ---
    ```

  - Follow it with an `# H1` title, for example `# Serve one model index from /v1/models, the dashboard, and nexus models`.
  - The body must state, in prose: the three current sources (the discovery catalog introduced by C2; config `models` with a `providers.DefaultModels` fallback in `internal/dashboard/playground.go:30-53`; config plus the `MapModel` Claude-tier mapping in `cmd/nexus/main.go:373-394`, which never consults `DefaultModels`); the concrete inconsistency that follows from them; and the next concrete action, which is to route all three through one index and to decide where it lives so the proxy and dashboard can both reach it without an import cycle (the catalog currently sits in `internal/proxy`, which the dashboard does not import; `internal/providers` or a new `internal/catalog` package are the obvious candidates, and the choice needs checking against the dashboard's existing dependencies).
  - Link this plan by relative path: `[2026-11-17 model catalog and id-aware routing](../plans/2026-11-17-model-catalog-and-id-routing.md)`.
  - Do not mirror plan status, checklists, or implementation notes into the ticket; it is intake, not a progress log.
  - Optional validation if the ticketing CLI is available on the machine: `/Users/av/sources/pi-test/skills/ticketing/scripts/ticket show catalog-single-index` from the repository root.
- **Prerequisites:** P1.
- **Verify:** `docs/tickets/catalog-single-index.md` exists; the five frontmatter keys appear in the documented order with `status: open`; the file has an `# H1` title; the body names all three surfaces by file, states the inconsistency, names the next action, and links this plan. If the ticketing CLI is available, `show` prints it and `list` includes it.

## Completion Log

| Date | Item | Notes |
| --- | --- | --- |
| 2026-11-17 | R1 | Implemented in `a5a23c7` (stable partition + `SetProviderModels`, `manualChain` folded into the shared helper), corrected by `028f84e` (read the model maps under the router lock) and `9b65335` (`ProviderServes` locked accessor, accurate `preferModel` comment). Tests: `internal/router/partition_test.go`, 25 tests covering every strategy, both no-op cases, group-order stability, unknown-name tolerance, replace-not-merge, and two `-race` concurrency tests. `go test ./internal/router/ -v -race` pass; `make test` 8/8 packages pass. Review round 1 REQUEST_CHANGES (the exported `Models` map escaped the lock, and P1's routability invariant would race with C2's discovery loop) — fixed in the two correction commits; round 2 APPROVE with optional findings only. Deviations: no debug log when `SetProviderModels` sees an unregistered name (the package has no logger; the tolerance is documented in the code comment). `make lint` could not run: `golangci-lint` is not installed in this environment; `go vet ./...` is clean. |
| 2026-11-17 | C1 | Implemented in `ba8f14e` (`ListModels` + `ErrNoModelList` + a per-family recipe struct), corrected by `5126b15` (dispatch on the concrete provider type instead of `Name()`, which is the operator's instance name for config-driven providers and silently sent enterprise families to the wrong endpoint and a custom provider named `gemini` to Google; and scrub the request URL out of transport errors, which carried the Gemini query key) and `e80727d` (bounded pagination for Anthropic and Gemini, tests pinning the `overridden` unwrapping, a total `withoutURL`, labels no longer URLs). Review: round 1 REQUEST_CHANGES (both defects above), round 2 REQUEST_CHANGES (the wrapper unwrapping the fix exists to protect was untested), round 3 APPROVE. Family table: default OpenAI-compatible `BaseURL()` + `/models`; anthropic `api.anthropic.com/v1/models` with `x-api-key`; gemini `v1beta/models?key=` with the `models/` prefix stripped; ollama `<baseURL>/api/tags`; groq an explicit `/v1/models` because its `BaseURL()` stops at `/openai`; perplexity, azure, vertex and bedrock return `ErrNoModelList`. `go test ./internal/providers/ -v -race` 49 pass; `make test` 8/8. |
| 2026-11-17 | C2 | Implemented in `d390b4f` (`internal/proxy/catalog.go`: `modelCatalog` with TTL, per-provider backoff 30s to 5m, single-flight, stable dedup snapshot, `peekKey`, and a `[catalog]` section defaulting to enabled with a 300s TTL via `config.Default()` pre-seeding), corrected by `e26a14c` (register seeded ids with the router, pin single-flight, extract `keyLocked` so `pickKey`/`peekKey` cannot drift), `1f7e0bf` (build the seed map in the registration loop, because joining by lowercased name dropped the operator's `models` for a built-in registered under an instance name; and publish the entry and the router index atomically under `modelCatalog.mu` then `Router.mu`), `d779981` (a successful but empty listing is now nothing learned: it no longer wipes the seed or its router registration, which would have armed a routability failure in exactly the Ollama-with-no-models case D-2 exists for; duplicate family registrations collapse to the first entry) and `6a3f7c9` (every `NewHandler` fixture scrubs `config.EnvKeyVars()`, after the seeded partition and warm loop turned an un-scrubbed fixture into live calls to api.anthropic.com carrying the developer's real key), plus `2cba6fd` for two comment corrections. Review: five rounds, each closing a real defect, final verdict APPROVE. Deliberate behaviour change recorded for D1: two config entries of the same provider family now collapse to the first, with a startup warning; previously the router held two same-named entries while `active` kept only the last. `go test ./internal/proxy/ -run TestCatalog -v -race` 27 pass; full package 197 pass; `make test` 8/8. |
| 2026-11-17 | P1 | Implemented in `4483f70` (`HandleModels` serves `modelCatalog.snapshot()` through the `modelIDs()` ladder, the dead `seen` map is gone, no `created` field, `owned_by: nexus`; new `Router.PreferModel` re-applies the partition after `stickyReorder` in `HandleMessages` and on the cascade chain), corrected by `1de67ea` (routability guard widened from 2 to all 5 strategies; `preferModel`'s comment rewritten because P1 had falsified it; `modelIDs` returns a copy of the stub slice; a test pinning the every-listing-fails rung) and `4b2c324` (the stale sticky NOTE in `HandleMessages`, false since R1's partition, rewritten; the cascade `PreferModel` pinned by a removal-sensitive test; the cache-serving test now populates the cache before asserting). Review: two REQUEST_CHANGES rounds, then APPROVE. **D-6 composition, as it landed:** `RouteChain` keeps its internal partition, and `HandleMessages` re-applies it via `PreferModel` after `stickyReorder`, so sticky decides only among serving providers. `HandleChatCompletions` needed no post-step — it has no sticky reorder, and its later steps are order-preserving filters. The cascade chain needed it because `CascadeChain` bypasses `RouteChain` entirely. **Deviations recorded for D1:** (a) D-9's rungs two and three are globally gated, but the seed folds them per provider (`pc.Models` else `DefaultModels`), so a mixed config advertises declared ids *and* family defaults where the literal ladder would stop at rung two; the per-provider reading is the better one and routability holds either way. (b) The stub rung is unroutable by construction when a provider is registered but the catalog is empty (an unknown family returns no `DefaultModels`); the routability invariant therefore covers the discovered and seeded cases. (c) Duplicate-family collapse (from C2) is a behaviour change D1 should mention. `go test ./internal/proxy/ -v -race` 233 pass; `make test` 8/8. |
