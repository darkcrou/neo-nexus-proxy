// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NEXUS contributors

package proxy

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/lynuxis2026-pixel/nexus-proxy/internal/config"
	"github.com/lynuxis2026-pixel/nexus-proxy/internal/providers"
	"github.com/lynuxis2026-pixel/nexus-proxy/internal/router"
)

// Model-catalog timing. A failed listing backs off exponentially from
// catalogInitialBackoff to catalogMaxBackoff, mirroring the key-cooldown
// reschedule philosophy in handler.go: retry a briefly broken endpoint soon,
// stop hammering one that is genuinely out.
const (
	catalogInitialBackoff = 30 * time.Second
	catalogMaxBackoff     = 5 * time.Minute
	// catalogMinTick floors the warm tick, so a tiny configured TTL cannot turn
	// the loop into a busy poll of every provider endpoint.
	catalogMinTick = 10 * time.Second
	// catalogTTLDivisor spreads refreshes: the loop ticks at ttl/5 so a provider
	// is fetched within the TTL without every provider being polled in lockstep.
	catalogTTLDivisor = 5

	// defaultCatalogTTL is the refresh interval when the operator configured none.
	defaultCatalogTTL = 300 * time.Second
)

// modelCatalog is the cached, background-refreshed index of the model ids each
// registered provider currently offers.
//
// It is a cache, never a request-path dependency: reads are a map scan under a
// read lock and never touch the network, while the warm loop fetches in the
// background. A successful fetch is pushed into the router with
// SetProviderModels, so RouteChain can select providers that serve the requested id.
type modelCatalog struct {
	mu        sync.RWMutex
	entries   map[string]*catalogEntry // keyed by provider name
	order     []string                 // provider names in registration order
	rt        *router.Router
	providers map[string]*activeProvider
	ttl       time.Duration
	now       func() time.Time // injected for tests

	// listModels is providers.ListModels. It is a field so tests can count
	// discovery attempts: a provider with no listing endpoint fails before any
	// request, so there is nothing on the wire to observe.
	listModels func(providers.Provider, string) ([]string, error)
}

// catalogEntry is one provider's last known state in the catalog.
type catalogEntry struct {
	models     []string
	fetchedAt  time.Time // when models/err were last established — by a success or a failure
	discovered bool      // at least one non-empty upstream listing has been received
	// err holds a retryable listing failure and nothing else: it is the marker
	// that switches the next attempt onto the backoff window instead of the TTL.
	// A provider with no listing endpoint is recorded in unsupported, not here,
	// so no entry carries two meanings and dueLocked's branches stay independent.
	err         string
	backoff     time.Duration
	inflight    bool
	unsupported bool // ErrNoModelList: recorded once, never scheduled again
}

// newModelCatalog builds a catalog over the registered providers. order is the
// registration order: Handler.providers is a map, so its iteration order is not
// stable and a stable snapshot needs the order captured once, at construction.
func newModelCatalog(rt *router.Router, active map[string]*activeProvider, order []string, ttl time.Duration) *modelCatalog {
	if ttl <= 0 {
		ttl = defaultCatalogTTL
	}
	c := &modelCatalog{
		entries:    make(map[string]*catalogEntry, len(order)),
		order:      append([]string(nil), order...),
		rt:         rt,
		providers:  active,
		ttl:        ttl,
		now:        time.Now,
		listModels: providers.ListModels,
	}
	for _, name := range c.order {
		c.entries[name] = &catalogEntry{}
	}
	return c
}

// seed primes the catalog with a per-provider list, so it is useful before the
// first fetch completes. fetchedAt stays zero, so the first tick still fetches:
// the seed is a fallback, not a claim about the upstream.
//
// The seed is also registered with the router, because it is the only id source
// in three states: warming disabled, before the first fetch lands, and
// permanently for a provider that exposes no listing endpoint. Without the push
// the catalog would advertise ids that Router.ProviderServes reports as served
// by nobody, and a request naming one of them would 404 at the first provider.
//
// models is keyed by the registry name, activeProvider.impl.Name(), which is what
// this catalog and the router are indexed by — not the operator's config name,
// which can differ (`nexus add groq-us sk-… --type groq` registers as "groq").
// NewHandler builds the map inside its registration loop, where both names are in
// scope, rather than re-deriving the pairing from config later.
func (c *modelCatalog) seed(models map[string][]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, name := range c.order {
		e := c.entries[name]
		if e == nil {
			e = &catalogEntry{}
			c.entries[name] = e
		}
		e.models = append([]string(nil), models[name]...)
		if len(e.models) == 0 {
			continue
		}
		// Lock order: modelCatalog.mu → Router.mu. Nesting is safe because no path
		// takes c.mu while holding the router lock — the router never calls back
		// into the catalog, and request paths take the two locks sequentially.
		// Both writes under c.mu keep a snapshot from naming ids the router does
		// not serve yet.
		c.rt.SetProviderModels(name, e.models)
	}
}

// snapshot returns the deduplicated union of the last known model lists, ordered
// by provider registration order and then by each provider's own list order, so
// repeated calls are byte-identical. It never fetches and never blocks beyond the
// read lock.
func (c *modelCatalog) snapshot() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	seen := make(map[string]struct{})
	var out []string
	for _, name := range c.order {
		e := c.entries[name]
		if e == nil {
			continue
		}
		for _, id := range e.models {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out
}

// missing reports absence only when every provider has a usable live listing.
// Seeds and partial discovery are useful for routing known ids, but cannot
// establish that an unlisted id is unavailable everywhere.
func (c *modelCatalog) missing(id string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.entries) == 0 || c.rt.HasModel(id) {
		return false
	}
	for _, e := range c.entries {
		if !e.discovered || e.err != "" {
			return false
		}
	}
	return true
}

// probeAlternative cycles through a provider's other advertised models if the
// model that originally cooled a key can no longer be used for recovery.
func (c *modelCatalog) probeAlternative(provider, exclude string, next int) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry := c.entries[provider]
	if entry == nil {
		return ""
	}
	for offset := range entry.models {
		id := entry.models[(next+offset)%len(entry.models)]
		if id != exclude {
			return id
		}
	}
	return ""
}

// refreshAll fetches every provider whose entry is due: not in flight, not known
// to have no listing endpoint, and past its wait window. The lock is released
// around the network call, so a slow upstream delays only its own refresh, never
// a snapshot.
func (c *modelCatalog) refreshAll(ctx context.Context) {
	for _, name := range c.order {
		if err := ctx.Err(); err != nil {
			return
		}
		ap := c.providers[name]
		if ap == nil {
			continue
		}

		c.mu.Lock()
		e := c.entries[name]
		if e == nil {
			e = &catalogEntry{}
			c.entries[name] = e
		}
		if !c.dueLocked(e, c.now()) {
			c.mu.Unlock()
			continue
		}
		e.inflight = true
		c.mu.Unlock()

		// Discovery must not perturb sticky key selection (D-7): peekKey reports
		// the key pickKey would use without adopting it. A provider whose whole
		// pool is cooling is still listed on its primary key — a listing is a
		// cheap GET that consumes no chat quota, and if the key is rejected the
		// listing fails and backs off like any other failure.
		key := ap.discoveryKey()
		ids, err := c.listModels(ap.impl, key)

		c.mu.Lock()
		e.inflight = false
		switch {
		case err == nil && len(ids) == 0:
			// A successful listing of nothing is "nothing learned", not "nothing
			// exists". An empty list is not evidence that previously reported ids
			// stopped existing — a local Ollama with no model pulled answers
			// /api/tags with exactly that — and the configured seed is the documented
			// fallback. Wiping the entry and the router index here would advertise
			// ids nothing serves, because the endpoint re-serves the seed from the
			// config/default ladder while the router index had been cleared. Keep both
			// untouched, but still clear the failure state and restamp, so the TTL
			// cadence is unchanged.
			e.err = ""
			e.backoff = 0
			e.fetchedAt = c.now()
			c.mu.Unlock()
			log.Debug().Str("provider", name).Msg("Model listing returned no models — keeping the known models")
		case err == nil:
			e.models = append([]string(nil), ids...)
			e.discovered = true
			e.err = ""
			e.backoff = 0
			// Lock order: modelCatalog.mu → Router.mu, safe for the reason given in
			// seed. The entry and the router index are published as one unit, so a
			// snapshot never names ids the router does not serve yet.
			c.rt.SetProviderModels(name, ids)
			// Stamped after the push, inside the same critical section: the recorded
			// time then marks when the list became live, so a reader that sees these
			// ids and this timestamp can rely on the router already serving them.
			e.fetchedAt = c.now()
			c.mu.Unlock()
			log.Debug().Str("provider", name).Int("models", len(ids)).Msg("Model catalog refreshed")
		case errors.Is(err, providers.ErrNoModelList):
			// Nothing to fetch, ever: record it once and stop scheduling this
			// provider. Its configured/seed list stays in the snapshot. The sentinel
			// is logged, not stored in err, which is reserved for retryable failures;
			// any earlier failure state is cleared because no retry is pending.
			e.unsupported = true
			e.err = ""
			e.fetchedAt = c.now()
			c.mu.Unlock()
			log.Debug().Str("provider", name).Err(providers.ErrNoModelList).
				Msg("Provider exposes no model listing — keeping its configured models")
		default:
			// Keep the last good list; the failure only affects the next attempt.
			e.err = err.Error()
			e.fetchedAt = c.now()
			e.backoff = nextCatalogBackoff(e.backoff)
			backoff := e.backoff
			c.mu.Unlock()
			log.Warn().Str("provider", name).Err(err).Dur("retry_in", backoff).Msg("Model listing failed — keeping the last known models")
		}
	}
}

// dueLocked reports whether a provider should be fetched now. A healthy provider
// waits for the TTL since its last fetch; a provider whose last attempt failed
// waits for its backoff instead, so a broken endpoint is retried on the backoff
// cadence (30s, 60s, … capped) rather than on every tick. Caller holds mu.
func (c *modelCatalog) dueLocked(e *catalogEntry, now time.Time) bool {
	if e.inflight || e.unsupported {
		return false
	}
	wait := c.ttl
	if e.err != "" {
		wait = e.backoff
		if wait <= 0 {
			wait = catalogInitialBackoff
		}
	}
	return !now.Before(e.fetchedAt.Add(wait))
}

// nextCatalogBackoff doubles a failed listing's backoff, starting at
// catalogInitialBackoff and capped at catalogMaxBackoff.
func nextCatalogBackoff(prev time.Duration) time.Duration {
	next := prev * 2
	if next <= 0 || next < catalogInitialBackoff {
		return catalogInitialBackoff
	}
	if next > catalogMaxBackoff {
		return catalogMaxBackoff
	}
	return next
}

// run is the warm loop: one refresh immediately (so the catalog does not wait a
// full tick after start), then one per tick. It stops when ctx is cancelled or
// the handler's stopHealth channel is closed by Close.
//
// stop must be the channel value captured at start: Close nils Handler.stopHealth
// after closing it, so re-reading the field would hang the loop.
func (c *modelCatalog) run(ctx context.Context, stop <-chan struct{}) {
	select {
	case <-ctx.Done():
		return
	case <-stop:
		return
	default:
	}
	c.refreshAll(ctx)

	tick := c.ttl / catalogTTLDivisor
	if tick < catalogMinTick {
		tick = catalogMinTick
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-t.C:
			c.refreshAll(ctx)
		}
	}
}

// startCatalog builds and seeds the model catalog, then starts its warm loop when
// warming is enabled and at least one provider is registered. A disabled catalog
// is still built and seeded, so the endpoint keeps a usable fallback; it simply
// never fetches. seedFor holds the pre-fetch ids per registry provider name, as
// built by NewHandler's registration loop. The loop stops on the handler's
// stopHealth channel, which Close closes exactly once.
func (h *Handler) startCatalog(seedFor map[string][]string, cc config.Catalog, order []string) {
	h.catalog = newModelCatalog(h.router, h.providers, order, catalogTTL(cc))
	h.catalog.seed(seedFor)
	if !catalogWarms(cc.Enabled, len(h.providers)) {
		return
	}
	go h.catalog.run(context.Background(), h.stopHealth)
	log.Info().
		Int("providers", len(order)).
		Dur("ttl", h.catalog.ttl).
		Msg("Model catalog warming enabled — provider model lists are discovered in the background")
}

// catalogTTL is the refresh interval the operator configured, falling back to
// the default for an absent or non-positive ttl_seconds.
func catalogTTL(cc config.Catalog) time.Duration {
	if cc.TTLSeconds <= 0 {
		return defaultCatalogTTL
	}
	return time.Duration(cc.TTLSeconds) * time.Second
}

// catalogWarms reports whether the warm loop should run: at least one provider is
// registered and the operator has not switched the [catalog] section off.
// Polling every provider endpoint is network behaviour the operator must be able
// to disable, so a disabled catalog stays seed-only.
func catalogWarms(enabled bool, providerCount int) bool { return enabled && providerCount > 0 }

// discoveryKey returns the key a model listing should authenticate with. It uses
// peekKey, never pickKey: pickKey adopts the key it scans forward to, and
// discovery must not move the sticky key pointer. When every key in the pool is
// cooling, the primary api_key is used anyway (see refreshAll).
func (a *activeProvider) discoveryKey() string {
	if key, _, ok := a.peekKey(); ok {
		return key
	}
	return a.apiKey
}
