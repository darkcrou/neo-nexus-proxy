# Model catalog and model-id-aware routing — completion report

**Plan:** [2026-11-17 model catalog and id-aware routing](../plans/2026-11-17-model-catalog-and-id-routing.md)
**Shipped as:** v0.9.0
**Scope:** `internal/router`, `internal/providers`, `internal/proxy`, `internal/config`, documentation.

## What shipped

`GET /v1/models` used to answer with a hardcoded three-item list of Claude ids. It
now answers with the deduplicated union of the models the configured providers
actually report, refreshed in the background behind a TTL, and a request naming
one of those ids is routed to a provider that serves it.

Six units, each committed separately and independently reviewed:

| Unit | What it delivered |
| --- | --- |
| R1 | `RouteChain` partitions providers by whether they serve the requested model id, for every strategy. `SetProviderModels` and the locked `ProviderServes` accessor; `manualChain` folded into the shared helper. |
| C1 | `providers.ListModels` — one package-level function with a per-family recipe, plus the `ErrNoModelList` sentinel for families that cannot list. |
| C2 | `modelCatalog`: TTL refresh, per-provider backoff 30s→5m, single-flight, stable dedup snapshot, seed from config, `[catalog]` config section, non-mutating `peekKey` for discovery. |
| P1 | `HandleModels` serves the snapshot through the fallback ladder; `Router.PreferModel` re-applies the partition after `stickyReorder`. |
| D1 | `CLAUDE.md` section, `Critical Decisions` bullet, endpoint table and diagram rows, `CHANGELOG.md` entry. |
| T1 | `docs/tickets/catalog-single-index.md` — the three listing surfaces still disagree. |

## Decisions that shaped the outcome

**No `provider/model` prefix.** The model id stays the address; the router resolves
the destination from the candidates that serve it. This keeps the wire format
unchanged for every client and leaves `direct`'s "forward the requested model id
verbatim" promise intact, because only *provider choice* became id-aware. A
prefixed design (LiteLLM, OpenRouter style) was considered and rejected by the
human. **Do not reintroduce it** without a new human decision — the reasoning is
in `CLAUDE.md` under "Model Catalog & Model-ID Routing".

**Live discovery, not config.** Config is a startup snapshot with no reload path,
so discovery is the only way a dynamic provider — notably a local Ollama — is
reflected without a restart.

**Feed the existing router map.** `router.Provider.Models` existed and was dead in
production; the catalog populates it rather than building a parallel mechanism.
This follows the precedent recorded for the two-level cooldown and revived
`manualChain` as a side effect.

**Discovery must never perturb sticky key selection.** `pickKey` mutates the
sticky cursor when it scans forward, so discovery uses `peekKey` over a shared
`keyLocked` core. Without the shared core the two would have drifted.

## Deviations from the plan, and why

1. **The fallback ladder's middle rungs fold per provider.** The plan words rungs
   two and three as a global gate — configured `models` first, `DefaultModels`
   only if no provider declares any. The seed instead folds them per provider, so
   a mixed config advertises declared ids *and* family defaults. The per-provider
   reading keeps every provider's fallback visible and routability holds either
   way, because the seed is registered with the router.
2. **The stub rung is unroutable by construction** when a provider is registered
   but the catalog is empty, since an unknown family returns no `DefaultModels`.
   The routability guarantee therefore covers the discovered and seeded cases.
3. **Duplicate registrations collapse to the first**, with a startup warning. The
   collapse keys on the registered name, so built-in families collide while
   distinct custom and enterprise instances do not. This is a behaviour change:
   previously the router held two same-named entries while `active` kept the last.
4. **D1's named verification (`make lint`) was never run.** `golangci-lint` is not
   installed in the environment this work happened in; `go vet ./...` and
   `gofmt` on the touched files are clean, and the repository carries no
   `.golangci.yml`, so the default linter set is largely covered by `go vet`. This
   is recorded as an unmet criterion, not a passed one.

## What the reviews caught

Every review round found a real defect; none of the six units passed first time
except T1. The ones worth remembering:

- **Family dispatch keyed on `Name()`.** `Name()` is the family keyword for
  built-ins but the operator's instance name for config-driven providers. A
  Bedrock provider called `prod-use1` took the generic recipe (a GET on a URL
  that can never list, so the caller would back off forever), and a custom
  provider that happened to be called `gemini` would have sent its key to
  Google. Fixed by switching on the unwrapped concrete type.
- **The Gemini API key leaked through transport errors.** The listing URL carries
  `?key=`, and `*url.Error` embeds the URL, so a connection failure put the key
  in the warn log. Fixed by wrapping only the cause.
- **Seeded ids never reached the router**, leaving the plan's headline
  routability invariant unsatisfiable whenever discovery was disabled or had not
  run yet.
- **A successful but empty listing wiped the seed** — the exact Ollama-with-no-
  models case that motivated live discovery in the first place.
- **A test fixture started making live calls** to api.anthropic.com with the
  developer's real key once the seeded partition began preferring the
  env-discovered provider. Fixed by scrubbing every variable `DiscoverEnv`
  consults, from one source of truth.
- **Three comments were falsified by the code they described** — `preferModel`'s
  note about what runs after `RouteChain`, the sticky NOTE in `HandleMessages`,
  and D1's description of the rejected interface design. Documentation accuracy
  was treated as a blocking bar, because a comment that misstates the mechanism
  is worse than no comment.

## Follow-ups

- **T1 / `docs/tickets/catalog-single-index.md`** — `GET /v1/models`, the
  dashboard picker and the `nexus models` CLI read three different sources and
  disagree in both directions. Unifying them needs a home for the index that
  both the proxy and the dashboard can import.
- **`make lint`** — worth running once `golangci-lint` is available; `errcheck`,
  `staticcheck` and `unused` have never been applied to this tree.
- **Pre-existing formatting drift** — 20 files are not `gofmt`-clean at the time
  of this work. Untouched deliberately, to keep the diffs reviewable.
