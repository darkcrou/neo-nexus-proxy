---
status: open
priority: medium
created: 2026-11-17
started: null
completed: null
---

# Serve one model index from /v1/models, the dashboard, and nexus models

NEXUS now answers "which models do I have?" from three unrelated sources.

The proxy's `GET /v1/models` reads the discovery catalog built in
`internal/proxy/catalog.go` and served by `HandleModels` / `modelIDs` in
`internal/proxy/gateway.go`: the deduplicated union of what the configured
providers last reported live, cached with a TTL, with the configured `models`
and `providers.DefaultModels` folded in only as a startup seed.

The dashboard picker reads `GET /api/playground/models`
(`internal/dashboard/playground.go:30-53`), which loads the config on every
request and lists each provider's config `models`, falling back to
`providers.DefaultModels(name)` when that list is empty. It never touches the
discovery catalog.

The `nexus models` CLI (`cmd/nexus/main.go:373-394`) loads the config and prints
the `MapModel` Claude-tier mapping for each provider — what `claude-haiku-4-5`,
`claude-sonnet-4-6`, and `claude-opus-4-5` become on that provider. It reads
neither the discovery catalog nor `providers.DefaultModels`.

The three therefore disagree in both directions. A model the proxy accepts and
advertises — anything discovery found on a live endpoint, or anything an Ollama
instance grew since startup — can be absent from the dashboard picker and from
the CLI's output. Conversely, a config-only id the dashboard still shows can be
one the router will not route to, because nothing registered it as a provider
capability. A user reading the picker and a user reading the CLI get different
answers than the proxy gives them, and none of the three is wrong on its own
terms.

The next step is to route all three surfaces through one model index. That
requires deciding where the index lives so both the proxy and the dashboard can
reach it without an import cycle: the catalog currently sits in
`internal/proxy`, which the dashboard does not import. `internal/providers` and a
new `internal/catalog` package are the obvious candidates, and the choice has to
be checked against the dashboard's existing dependencies rather than assumed.

This follow-up was deliberately left out of
[2026-11-17 model catalog and id-aware routing](../plans/2026-11-17-model-catalog-and-id-routing.md),
which delivered the proxy-side catalog and the id-aware routing only.
