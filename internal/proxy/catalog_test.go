// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NEXUS contributors

package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lynuxis2026-pixel/nexus-proxy/internal/config"
	"github.com/lynuxis2026-pixel/nexus-proxy/internal/providers"
	"github.com/lynuxis2026-pixel/nexus-proxy/internal/router"
	"github.com/lynuxis2026-pixel/nexus-proxy/internal/storage"
)

// ─── test fixtures ──────────────────────────────────────────────────────────

// fakeUpstream is an OpenAI-compatible /models endpoint whose answer the test can
// change between fetches. It counts requests and records the last Authorization
// header, so the TTL, backoff, and key-selection gates are asserted on observable
// behaviour instead of on catalog internals.
type fakeUpstream struct {
	srv *httptest.Server

	mu     sync.Mutex
	ids    []string
	status int
	auth   string

	hits    atomic.Int64
	fetched chan struct{}
}

func newFakeUpstream(t *testing.T, ids ...string) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{ids: ids, status: http.StatusOK, fetched: make(chan struct{}, 1)}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.mu.Lock()
		ids, status := f.ids, f.status
		f.auth = r.Header.Get("Authorization")
		f.mu.Unlock()
		select {
		case f.fetched <- struct{}{}:
		default:
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		data := make([]map[string]string, 0, len(ids))
		for _, id := range ids {
			data = append(data, map[string]string{"id": id, "object": "model"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"object": "list", "data": data})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUpstream) requests() int64 { return f.hits.Load() }

func (f *fakeUpstream) serveModels(ids ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ids, f.status = ids, http.StatusOK
}

func (f *fakeUpstream) serveStatus(code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = code
}

func (f *fakeUpstream) lastAuth() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.auth
}

// newProviderAt builds a real OpenAI-compatible provider pointed at baseURL, with
// an optional key pool (the sticky-key path needs more than one key to be
// observable).
func newProviderAt(t *testing.T, name, baseURL string, keys ...string) *activeProvider {
	t.Helper()
	if len(keys) == 0 {
		keys = []string{"key-" + name}
	}
	impl, err := providers.New(providers.Spec{
		Name:    name,
		Type:    "openai-compatible",
		BaseURL: baseURL,
		Tier:    providers.TierFree,
	})
	if err != nil {
		t.Fatalf("build provider %s: %v", name, err)
	}
	return &activeProvider{
		impl:    impl,
		apiKey:  keys[0],
		keys:    keys,
		cool:    make([]time.Time, len(keys)),
		backoff: make([]time.Duration, len(keys)),
	}
}

// newUpstreamProvider builds a provider pointed at the fake upstream.
func newUpstreamProvider(t *testing.T, name string, f *fakeUpstream, keys ...string) *activeProvider {
	t.Helper()
	return newProviderAt(t, name, f.srv.URL, keys...)
}

// blockingUpstream is a /models endpoint that parks every request until the test
// releases it. It lets two refreshes be forced to overlap with channels alone,
// instead of racing them and hoping for a window.
type blockingUpstream struct {
	srv     *httptest.Server
	hits    atomic.Int64
	started chan struct{}
	release chan struct{}
}

func newBlockingUpstream(t *testing.T, ids ...string) *blockingUpstream {
	t.Helper()
	b := &blockingUpstream{started: make(chan struct{}, 1), release: make(chan struct{})}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.hits.Add(1)
		select {
		case b.started <- struct{}{}:
		default:
		}
		<-b.release
		data := make([]map[string]string, 0, len(ids))
		for _, id := range ids {
			data = append(data, map[string]string{"id": id, "object": "model"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"object": "list", "data": data})
	}))
	// Release before closing, so a failing test cannot hang in cleanup on a
	// handler that is still parked.
	t.Cleanup(func() {
		select {
		case <-b.release:
		default:
			close(b.release)
		}
		b.srv.Close()
	})
	return b
}

// fakeClock drives the catalog's TTL and backoff gates deterministically, so no
// test has to sleep.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// buildCatalog assembles a catalog the way NewHandler does: providers registered
// in slice order, one activeProvider each, and an injected clock.
func buildCatalog(t *testing.T, ttl time.Duration, aps ...*activeProvider) (*modelCatalog, *fakeClock) {
	t.Helper()
	rt := router.New(router.StrategyAuto)
	active := make(map[string]*activeProvider, len(aps))
	order := make([]string, 0, len(aps))
	for _, ap := range aps {
		name := ap.impl.Name()
		active[name] = ap
		order = append(order, name)
		rt.AddProvider(&router.Provider{Name: name, Tier: ap.impl.Tier(), Healthy: true})
	}
	clock := newFakeClock()
	cat := newModelCatalog(rt, active, order, ttl)
	cat.now = clock.Now
	return cat, clock
}

// entryState reads one provider's catalog state under the catalog lock. Tests
// assert on the recorded error and backoff here because the read surface
// (snapshot) deliberately hides them.
func entryState(t *testing.T, cat *modelCatalog, name string) (errText string, backoff time.Duration, unsupported bool) {
	t.Helper()
	cat.mu.RLock()
	defer cat.mu.RUnlock()
	e := cat.entries[name]
	if e == nil {
		t.Fatalf("no catalog entry for provider %q", name)
	}
	return e.err, e.backoff, e.unsupported
}

// roundIDs is the id set a provider offers at round r. Ids are only ever added, so
// an id that has been published stays served for the rest of the test and a
// routability check can never fail spuriously.
func roundIDs(name string, round int) []string {
	ids := make([]string, 0, round+1)
	for i := 0; i <= round; i++ {
		ids = append(ids, fmt.Sprintf("%s-m%d", name, i))
	}
	return ids
}

// newHandlerFromConfig builds a real Handler from a config file, so a test asserts
// on the wiring NewHandler performs (which pairs config entries with registry
// names) instead of on a hand-built catalog.
func newHandlerFromConfig(t *testing.T, tomlBody string) *Handler {
	t.Helper()
	// Environment discovery would add providers the test did not configure and
	// could shadow the ones it did.
	t.Setenv("GROQ_API_KEY", "")
	t.Setenv("DEEPSEEK_API_KEY", "")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(tomlBody), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	db, err := storage.New(filepath.Join(dir, "nexus.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	h, err := NewHandler(&Config{ConfigPath: path, DisableCache: true}, db, nil)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// ─── tests ──────────────────────────────────────────────────────────────────

func TestCatalogRefreshPopulatesSnapshot(t *testing.T) {
	f := newFakeUpstream(t, "alpha-model", "beta-model")
	cat, _ := buildCatalog(t, 300*time.Second, newUpstreamProvider(t, "acme", f))

	if got := cat.snapshot(); len(got) != 0 {
		t.Fatalf("an unseeded catalog must start empty, got %v", got)
	}

	cat.refreshAll(context.Background())

	want := []string{"alpha-model", "beta-model"}
	if got := cat.snapshot(); !reflect.DeepEqual(got, want) {
		t.Errorf("snapshot = %v, want %v", got, want)
	}
	if f.requests() != 1 {
		t.Errorf("upstream requests = %d, want 1", f.requests())
	}
}

func TestCatalogSkipsFetchWithinTTL(t *testing.T) {
	f := newFakeUpstream(t, "alpha-model")
	cat, clock := buildCatalog(t, 300*time.Second, newUpstreamProvider(t, "acme", f))

	cat.refreshAll(context.Background())
	clock.advance(299 * time.Second)
	cat.refreshAll(context.Background())

	if f.requests() != 1 {
		t.Errorf("upstream requests = %d, want 1 — a refresh inside the TTL must not reach the provider", f.requests())
	}
	if got := cat.snapshot(); !reflect.DeepEqual(got, []string{"alpha-model"}) {
		t.Errorf("snapshot = %v, want [alpha-model]", got)
	}
}

func TestCatalogRefetchesAfterTTL(t *testing.T) {
	f := newFakeUpstream(t, "alpha-model")
	cat, clock := buildCatalog(t, 300*time.Second, newUpstreamProvider(t, "acme", f))

	cat.refreshAll(context.Background())
	f.serveModels("alpha-model", "gamma-model")
	clock.advance(300 * time.Second)
	cat.refreshAll(context.Background())

	if f.requests() != 2 {
		t.Errorf("upstream requests = %d, want 2 — a refresh past the TTL must refetch", f.requests())
	}
	want := []string{"alpha-model", "gamma-model"}
	if got := cat.snapshot(); !reflect.DeepEqual(got, want) {
		t.Errorf("snapshot = %v, want %v (the refetched list replaces the old one)", got, want)
	}
}

func TestCatalogFailureKeepsLastGoodListAndBacksOff(t *testing.T) {
	f := newFakeUpstream(t, "alpha-model")
	cat, clock := buildCatalog(t, 300*time.Second, newUpstreamProvider(t, "acme", f))

	cat.refreshAll(context.Background())
	f.serveStatus(http.StatusInternalServerError)
	clock.advance(300 * time.Second) // the TTL alone would allow a retry now
	cat.refreshAll(context.Background())

	if f.requests() != 2 {
		t.Fatalf("upstream requests = %d, want 2", f.requests())
	}
	if got := cat.snapshot(); !reflect.DeepEqual(got, []string{"alpha-model"}) {
		t.Errorf("snapshot = %v, want [alpha-model] — a failed listing keeps the last good list", got)
	}
	errText, backoff, unsupported := entryState(t, cat, "acme")
	if errText == "" {
		t.Error("a failed listing must be recorded on the entry")
	}
	if backoff != catalogInitialBackoff {
		t.Errorf("backoff = %s, want %s", backoff, catalogInitialBackoff)
	}
	if unsupported {
		t.Error("a failed listing is not the same as a provider with no listing endpoint")
	}

	clock.advance(catalogInitialBackoff - time.Second)
	cat.refreshAll(context.Background())
	if f.requests() != 2 {
		t.Errorf("upstream requests = %d, want 2 — a failing provider must not be retried before its backoff expires", f.requests())
	}

	clock.advance(time.Second)
	cat.refreshAll(context.Background())
	if f.requests() != 3 {
		t.Errorf("upstream requests = %d, want 3 — the retry must happen once the backoff expires", f.requests())
	}
}

func TestCatalogRecoveryClearsErrorAndResetsBackoff(t *testing.T) {
	f := newFakeUpstream(t, "alpha-model")
	f.serveStatus(http.StatusBadGateway)
	cat, clock := buildCatalog(t, 300*time.Second, newUpstreamProvider(t, "acme", f))

	cat.refreshAll(context.Background())
	if _, backoff, _ := entryState(t, cat, "acme"); backoff != catalogInitialBackoff {
		t.Fatalf("backoff after the first failure = %s, want %s", backoff, catalogInitialBackoff)
	}

	f.serveModels("alpha-model")
	clock.advance(catalogInitialBackoff)
	cat.refreshAll(context.Background())

	errText, backoff, _ := entryState(t, cat, "acme")
	if errText != "" || backoff != 0 {
		t.Errorf("after recovery: err = %q, backoff = %s; want both cleared", errText, backoff)
	}
	if got := cat.snapshot(); !reflect.DeepEqual(got, []string{"alpha-model"}) {
		t.Errorf("snapshot = %v, want [alpha-model]", got)
	}

	// The cadence is back to the TTL: no retry before it, one retry at it.
	requests := f.requests()
	clock.advance(299 * time.Second)
	cat.refreshAll(context.Background())
	if f.requests() != requests {
		t.Errorf("upstream requests = %d, want %d — a recovered provider waits for the TTL again", f.requests(), requests)
	}
	clock.advance(time.Second)
	cat.refreshAll(context.Background())
	if f.requests() != requests+1 {
		t.Errorf("upstream requests = %d, want %d after the TTL", f.requests(), requests+1)
	}
}

func TestCatalogBackoffGrowsAndCaps(t *testing.T) {
	f := newFakeUpstream(t, "alpha-model")
	f.serveStatus(http.StatusInternalServerError)
	cat, clock := buildCatalog(t, 300*time.Second, newUpstreamProvider(t, "acme", f))

	want := []time.Duration{
		30 * time.Second,
		60 * time.Second,
		120 * time.Second,
		240 * time.Second,
		300 * time.Second, // capped at 5m
		300 * time.Second,
	}
	for i, wait := range want {
		cat.refreshAll(context.Background())
		if _, backoff, _ := entryState(t, cat, "acme"); backoff != wait {
			t.Fatalf("attempt %d: backoff = %s, want %s", i+1, backoff, wait)
		}
		requests := f.requests()
		clock.advance(wait - time.Second)
		cat.refreshAll(context.Background())
		if f.requests() != requests {
			t.Fatalf("attempt %d: retried %s after a %s backoff", i+1, wait-time.Second, wait)
		}
		clock.advance(time.Second)
	}
}

func TestCatalogDeduplicatesSharedModelID(t *testing.T) {
	first := newFakeUpstream(t, "shared-model", "first-only")
	second := newFakeUpstream(t, "second-only", "shared-model")
	cat, _ := buildCatalog(t, 300*time.Second,
		newUpstreamProvider(t, "first", first),
		newUpstreamProvider(t, "second", second),
	)

	cat.refreshAll(context.Background())

	want := []string{"shared-model", "first-only", "second-only"}
	if got := cat.snapshot(); !reflect.DeepEqual(got, want) {
		t.Errorf("snapshot = %v, want %v — an id offered by two providers appears once", got, want)
	}
}

func TestCatalogSnapshotOrderIsStable(t *testing.T) {
	// Registration order is deliberately the reverse of both the alphabetical and
	// the sorted-by-id order, and each list is internally unsorted, so any sorting
	// or map iteration would show up here.
	second := newFakeUpstream(t, "zebra", "yak")
	first := newFakeUpstream(t, "mango", "apple")
	cat, _ := buildCatalog(t, 300*time.Second,
		newUpstreamProvider(t, "second", second),
		newUpstreamProvider(t, "first", first),
	)

	cat.refreshAll(context.Background())

	want := []string{"zebra", "yak", "mango", "apple"}
	got := cat.snapshot()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot = %v, want %v (registration order, then each provider's own list order)", got, want)
	}
	for i := 0; i < 5; i++ {
		if again := cat.snapshot(); !reflect.DeepEqual(again, want) {
			t.Fatalf("snapshot %d = %v, want %v — the order must be stable across calls", i+2, again, want)
		}
	}
}

func TestCatalogUnsupportedProviderIsAttemptedOnce(t *testing.T) {
	// Perplexity publishes no listing endpoint, and Azure is Anthropic-native:
	// ListModels reports ErrNoModelList before any request leaves the process.
	perplexity, err := providers.New(providers.Spec{Name: "perplexity", APIKey: "pplx-key"})
	if err != nil {
		t.Fatalf("build perplexity: %v", err)
	}
	azureUpstream := newFakeUpstream(t, "unreachable")
	azure, err := providers.New(providers.Spec{
		Name: "azure-east", Type: "azure", BaseURL: azureUpstream.srv.URL, APIKey: "az-key",
	})
	if err != nil {
		t.Fatalf("build azure: %v", err)
	}
	cat, clock := buildCatalog(t, 300*time.Second,
		&activeProvider{impl: perplexity, apiKey: "pplx-key", keys: []string{"pplx-key"}, cool: make([]time.Time, 1), backoff: make([]time.Duration, 1)},
		&activeProvider{impl: azure, apiKey: "az-key", keys: []string{"az-key"}, cool: make([]time.Time, 1), backoff: make([]time.Duration, 1)},
	)

	var attempts atomic.Int64
	cat.listModels = func(p providers.Provider, apiKey string) ([]string, error) {
		attempts.Add(1)
		return providers.ListModels(p, apiKey)
	}
	cat.seed(map[string][]string{"perplexity": {"sonar", "sonar-pro"}, "azure-east": {"gpt-4o"}})

	cat.refreshAll(context.Background())
	clock.advance(24 * time.Hour)
	cat.refreshAll(context.Background())

	if got := attempts.Load(); got != 2 {
		t.Errorf("listing attempts = %d, want 2 — one per unsupported provider, then never again", got)
	}
	if azureUpstream.requests() != 0 {
		t.Errorf("azure upstream requests = %d, want 0 — an unsupported provider must never be probed", azureUpstream.requests())
	}
	for _, name := range []string{"perplexity", "azure-east"} {
		if _, _, unsupported := entryState(t, cat, name); !unsupported {
			t.Errorf("%s: entry is not marked unsupported", name)
		}
	}
	if got := cat.snapshot(); !reflect.DeepEqual(got, []string{"sonar", "sonar-pro", "gpt-4o"}) {
		t.Errorf("snapshot = %v, want the seeded lists of both unsupported providers", got)
	}
}

func TestCatalogEmptyListingKeepsSeed(t *testing.T) {
	// A healthy endpoint that lists nothing — a local Ollama with no model pulled
	// answers /api/tags with an empty list — is a success, so it must not be read as
	// "the previously reported ids are gone". The seed is the documented fallback
	// and the endpoint re-serves it, so clearing the router index here would
	// advertise ids that nothing serves.
	f := newFakeUpstream(t) // answers {"data":[]}
	cat, clock := buildCatalog(t, 300*time.Second, newUpstreamProvider(t, "acme", f))
	cat.seed(map[string][]string{"acme": {"configured-model"}})

	cat.refreshAll(context.Background())

	if f.requests() != 1 {
		t.Fatalf("upstream requests = %d, want 1 — the empty listing must still be attempted", f.requests())
	}
	if got := cat.snapshot(); !reflect.DeepEqual(got, []string{"configured-model"}) {
		t.Errorf("snapshot = %v, want [configured-model] — an empty listing is not evidence the seeded ids stopped existing", got)
	}
	if !cat.rt.ProviderServes("acme", "configured-model") {
		t.Error("the router no longer serves the seeded id after an empty listing")
	}
	errText, backoff, unsupported := entryState(t, cat, "acme")
	if errText != "" || backoff != 0 {
		t.Errorf("after an empty listing: err = %q, backoff = %s; want both clear — it is not a failure", errText, backoff)
	}
	if unsupported {
		t.Error("an empty listing is not a provider with no listing endpoint")
	}

	// The TTL cadence is unchanged: the empty answer is still a completed fetch.
	clock.advance(299 * time.Second)
	cat.refreshAll(context.Background())
	if f.requests() != 1 {
		t.Errorf("upstream requests = %d, want 1 — an empty listing must still stamp fetchedAt", f.requests())
	}
	clock.advance(time.Second)
	cat.refreshAll(context.Background())
	if f.requests() != 2 {
		t.Errorf("upstream requests = %d, want 2 — the empty listing must not skip the next TTL refresh", f.requests())
	}
}

func TestCatalogDuplicateRegistrationCollapsesToFirst(t *testing.T) {
	// Two config entries of one family report the same registry name. The router
	// keys by that name and SetProviderModels/ProviderServes stop at the first
	// match, so a second entry would carry no models and be partitioned away. The
	// registration loop skips it entirely: one entry everywhere, the first one.
	h := newHandlerFromConfig(t, `
[catalog]
enabled = false

[[providers]]
name = "groq-us"
type = "groq"
api_key = "gsk-us"
models = ["first-model"]

[[providers]]
name = "groq-eu"
type = "groq"
api_key = "gsk-eu"
models = ["second-model"]
`)

	if got := len(h.providers); got != 1 {
		t.Errorf("registered providers = %d, want 1", got)
	}
	if got := h.catalog.snapshot(); !reflect.DeepEqual(got, []string{"first-model"}) {
		t.Errorf("snapshot = %v, want [first-model] — one seed, from the first entry", got)
	}

	registered := 0
	for _, p := range h.router.Providers() {
		if p.Name == "groq" {
			registered++
		}
	}
	if registered != 1 {
		t.Errorf("router entries named groq = %d, want 1 — a duplicate would never receive its models", registered)
	}

	chain := h.router.RouteChain("first-model", router.ComplexitySimple)
	if len(chain) != 1 || chain[0].Name != "groq" {
		t.Errorf("chain = %v, want exactly one groq slot", chain)
	}
	if !h.router.ProviderServes("groq", "first-model") {
		t.Error("the router does not serve the first entry's configured id")
	}
}

func TestCatalogExhaustedKeyPoolListsOnPrimaryKey(t *testing.T) {
	// With every key cooling, peekKey reports ok=false. Discovery still lists, on
	// the provider's primary api_key: a listing is a cheap GET that consumes no
	// chat quota, and a rejected key fails and backs off like any other failure.
	f := newFakeUpstream(t, "alpha-model")
	ap := newUpstreamProvider(t, "acme", f, "k0", "k1")
	for i := range ap.cool {
		ap.cool[i] = time.Now().Add(time.Hour)
	}
	cat, _ := buildCatalog(t, 300*time.Second, ap)

	if _, _, ok := ap.peekKey(); ok {
		t.Fatal("peekKey reports a usable key with the whole pool cooling")
	}

	cat.refreshAll(context.Background())

	if got, want := f.lastAuth(), "Bearer "+ap.apiKey; got != want {
		t.Errorf("listing authenticated as %q, want %q — an exhausted pool lists on the primary key", got, want)
	}
	if got := cat.snapshot(); !reflect.DeepEqual(got, []string{"alpha-model"}) {
		t.Errorf("snapshot = %v, want [alpha-model]", got)
	}
}

func TestCatalogSuccessReachesRouter(t *testing.T) {
	f := newFakeUpstream(t, "alpha-model")
	cat, _ := buildCatalog(t, 300*time.Second, newUpstreamProvider(t, "acme", f))

	cat.refreshAll(context.Background())

	if !cat.rt.ProviderServes("acme", "alpha-model") {
		t.Error("the router does not report the discovered id as served")
	}
	if cat.rt.ProviderServes("acme", "never-listed") {
		t.Error("the router reports an id the provider never listed")
	}
}

func TestCatalogDiscoveryDoesNotPerturbStickyKey(t *testing.T) {
	f := newFakeUpstream(t, "alpha-model")
	ap := newUpstreamProvider(t, "acme", f, "k0", "k1", "k2")
	ap.rr = 1
	ap.cool[1] = time.Now().Add(time.Hour) // the sticky current key is cooling
	cat, _ := buildCatalog(t, 300*time.Second, ap)

	// peekKey reports the key pickKey would adopt, without adopting it.
	if key, idx, ok := ap.peekKey(); !ok || key != "k2" || idx != 2 {
		t.Fatalf("peekKey = (%q, %d, %v), want (k2, 2, true)", key, idx, ok)
	}
	if ap.rr != 1 {
		t.Fatalf("peekKey moved the sticky key pointer to %d, want 1", ap.rr)
	}

	cat.refreshAll(context.Background())

	if ap.rr != 1 {
		t.Errorf("sticky key pointer = %d after discovery, want 1 — listing must not change key selection", ap.rr)
	}
	if got := f.lastAuth(); got != "Bearer k2" {
		t.Errorf("listing used %q, want %q — discovery must list on the key pickKey would use", got, "Bearer k2")
	}
}

func TestCatalogConcurrentSnapshotsDuringRefresh(t *testing.T) {
	f := newFakeUpstream(t, "alpha", "beta", "gamma")
	cat, _ := buildCatalog(t, 300*time.Second, newUpstreamProvider(t, "acme", f))
	cat.refreshAll(context.Background())

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				cat.snapshot()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		cat.refreshAll(context.Background())
	}()
	wg.Wait()

	if got := cat.snapshot(); !reflect.DeepEqual(got, []string{"alpha", "beta", "gamma"}) {
		t.Errorf("snapshot = %v after concurrent refreshes", got)
	}
}

func TestCatalogSingleFlightSkipsOverlappingRefresh(t *testing.T) {
	up := newBlockingUpstream(t, "alpha-model")
	cat, _ := buildCatalog(t, 300*time.Second, newProviderAt(t, "acme", up.srv.URL))

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		cat.refreshAll(context.Background())
	}()

	// The first refresh is now parked inside the HTTP call, past the point where
	// it marks its entry in flight.
	select {
	case <-up.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first refresh never reached the upstream")
	}

	// A second, overlapping refresh must return without fetching.
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		cat.refreshAll(context.Background())
	}()

	select {
	case <-secondDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the second refresh blocked instead of skipping the in-flight provider")
	}

	if got := up.hits.Load(); got != 1 {
		t.Errorf("upstream requests = %d while the first refresh is in flight, want 1 — single flight must prevent a stampede", got)
	}

	close(up.release)
	select {
	case <-firstDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the first refresh did not complete after the upstream was released")
	}

	if got := up.hits.Load(); got != 1 {
		t.Errorf("upstream requests = %d after both refreshes, want 1 — the skipped refresh must not retry", got)
	}
	if got := cat.snapshot(); !reflect.DeepEqual(got, []string{"alpha-model"}) {
		t.Errorf("snapshot = %v, want [alpha-model] — the single fetch must still populate the catalog", got)
	}
}

func TestCatalogSeedIsVisibleBeforeFirstFetch(t *testing.T) {
	f := newFakeUpstream(t, "live-model")
	cat, _ := buildCatalog(t, 300*time.Second, newUpstreamProvider(t, "acme", f))
	cat.seed(map[string][]string{"acme": {"configured-model"}})

	if got := cat.snapshot(); !reflect.DeepEqual(got, []string{"configured-model"}) {
		t.Errorf("snapshot = %v, want [configured-model] before any fetch", got)
	}
	if f.requests() != 0 {
		t.Fatalf("upstream requests = %d, want 0 — seeding must not fetch", f.requests())
	}

	// The seed is a fallback, not a claim: the first tick still fetches.
	cat.refreshAll(context.Background())
	if f.requests() != 1 {
		t.Errorf("upstream requests = %d, want 1 — a seeded catalog still fetches on its first tick", f.requests())
	}
	if got := cat.snapshot(); !reflect.DeepEqual(got, []string{"live-model"}) {
		t.Errorf("snapshot = %v, want the discovered [live-model]", got)
	}
}

func TestCatalogSeedRegistersModelsWithRouter(t *testing.T) {
	f := newFakeUpstream(t, "live-model")
	cat, _ := buildCatalog(t, 300*time.Second, newUpstreamProvider(t, "acme", f))

	cat.seed(map[string][]string{"acme": {"configured-model"}})

	if !cat.rt.ProviderServes("acme", "configured-model") {
		t.Error("the router does not report the seeded id as served — an advertised id must be routable before the first fetch")
	}
	if cat.rt.ProviderServes("acme", "live-model") {
		t.Error("the router reports an id nothing has discovered or seeded")
	}
	if f.requests() != 0 {
		t.Errorf("upstream requests = %d, want 0 — registering the seed must not fetch", f.requests())
	}
}

func TestCatalogSeedFallsBackToDefaultModels(t *testing.T) {
	// The seed is the middle rungs of the endpoint's ladder: configured models, else
	// the provider family's default list.
	h := newHandlerFromConfig(t, `
[catalog]
enabled = false

[[providers]]
name = "groq"
type = "groq"
api_key = "gsk-test"
models = ["configured-model"]

[[providers]]
name = "deepseek"
api_key = "sk-test"
`)

	want := append([]string{"configured-model"}, providers.DefaultModels("deepseek")...)
	if got := h.catalog.snapshot(); !reflect.DeepEqual(got, want) {
		t.Errorf("snapshot = %v, want %v — a provider without configured models seeds from its family defaults", got, want)
	}
}

func TestCatalogSeedKeepsModelsOfRenamedBuiltinProvider(t *testing.T) {
	// `nexus add groq-us sk-… --type groq` is an accepted config shape: the config
	// name differs from the registry name the family provider reports. The seed must
	// follow the registry name and keep the configured ids; pairing the two by config
	// name would fall through to the Groq defaults, so the catalog would advertise ids
	// nobody configured and the configured id would not be routable.
	h := newHandlerFromConfig(t, `
[catalog]
enabled = false

[[providers]]
name = "groq-us"
type = "groq"
api_key = "gsk-test"
models = ["custom-model"]
`)

	if got := h.catalog.snapshot(); !reflect.DeepEqual(got, []string{"custom-model"}) {
		t.Errorf("snapshot = %v, want [custom-model] — the configured ids must survive the name difference", got)
	}
	if !h.router.ProviderServes("groq", "custom-model") {
		t.Error("the router does not report the configured id as served")
	}
	for _, id := range providers.DefaultModels("groq") {
		if h.router.ProviderServes("groq", id) {
			t.Errorf("router serves the family default %q, which nobody configured", id)
		}
	}
}

// roundUpstream is a /models endpoint that answers with the round the test has
// staged and records which round it actually served. That record is what lets the
// catalog's own clock double as a probe: when the catalog stamps a fetch, the ids
// of the round the upstream just served must already be routable.
type roundUpstream struct {
	srv *httptest.Server
	mu  sync.Mutex
	// staged is the round the test has set; served maps a provider name to the round
	// this endpoint last answered with.
	staged int
	served map[string]int
}

func newRoundUpstream(t *testing.T, name string) *roundUpstream {
	t.Helper()
	u := &roundUpstream{served: make(map[string]int)}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		round := u.staged
		u.served[name] = round
		u.mu.Unlock()
		data := make([]map[string]string, 0, round+1)
		for _, id := range roundIDs(name, round) {
			data = append(data, map[string]string{"id": id, "object": "model"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"object": "list", "data": data})
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *roundUpstream) stage(round int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.staged = round
}

func (u *roundUpstream) servedRound(name string) (int, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	round, ok := u.served[name]
	return round, ok
}

func TestCatalogPublishesSnapshotAndRouterTogether(t *testing.T) {
	// A snapshot may only name ids the router already serves. While the entry and the
	// router index were written in two steps, a reader could observe a provider's new
	// list while the router still held the old one — the half-state that makes an
	// advertised id unroutable.
	//
	// Two observations, one deterministic and one concurrent: the injected clock is
	// called inside the critical section that publishes a list, so probing the router
	// from it sees the exact contract at that instant (the racy variant alone cannot
	// see a nanosecond-wide reorder, because a router read that starts after the entry
	// write blocks on the pending push and returns the fresh state). Rounds only add
	// ids, so an id observed once stays served and neither check can fail spuriously.
	const (
		providerCount = 3
		rounds        = 20
		readers       = 6
	)

	ups := make([]*roundUpstream, providerCount)
	aps := make([]*activeProvider, providerCount)
	seed := make(map[string][]string, providerCount)
	names := make([]string, providerCount)
	for i := 0; i < providerCount; i++ {
		name := fmt.Sprintf("p%d", i)
		names[i] = name
		ups[i] = newRoundUpstream(t, name)
		aps[i] = newProviderAt(t, name, ups[i].srv.URL)
		seed[name] = roundIDs(name, 0)
	}
	cat, clock := buildCatalog(t, 300*time.Second, aps...)
	cat.seed(seed)

	var probeErr error
	cat.now = func() time.Time {
		for i, name := range names {
			round, ok := ups[i].servedRound(name)
			if !ok {
				continue
			}
			for _, id := range roundIDs(name, round) {
				if !cat.rt.ProviderServes(name, id) {
					probeErr = fmt.Errorf("%q was recorded as fetched while the router did not serve it — the entry and the router index must be published together", id)
				}
			}
		}
		return clock.Now()
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				for _, id := range cat.snapshot() {
					owner, _, ok := strings.Cut(id, "-m")
					if !ok {
						t.Errorf("snapshot returned a malformed id %q", id)
						return
					}
					if !cat.rt.ProviderServes(owner, id) {
						t.Errorf("snapshot advertised %q while the router did not serve it — the entry and the router index must be published together", id)
						return
					}
				}
			}
		}()
	}

	for round := 1; round <= rounds; round++ {
		for i := range ups {
			ups[i].stage(round)
		}
		clock.advance(300 * time.Second)
		cat.refreshAll(context.Background())
	}
	close(done)
	wg.Wait()

	if probeErr != nil {
		t.Error(probeErr)
	}
	want := make([]string, 0, providerCount*(rounds+1))
	for i := 0; i < providerCount; i++ {
		want = append(want, roundIDs(names[i], rounds)...)
	}
	if got := cat.snapshot(); !reflect.DeepEqual(got, want) {
		t.Errorf("snapshot = %v, want every id of the last round (%d ids)", got, len(want))
	}
}

func TestCatalogDisabledStartsNoWarmLoop(t *testing.T) {
	// The guard is the whole decision: no provider registered, or the operator
	// switched the [catalog] section off, means no background polling.
	if catalogWarms(false, 3) {
		t.Error("a disabled catalog must not warm")
	}
	if catalogWarms(true, 0) {
		t.Error("a catalog with no provider must not warm")
	}
	if !catalogWarms(true, 3) {
		t.Error("an enabled catalog with providers must warm")
	}

	// A disabled catalog is still built and seeded, so the endpoint keeps a usable
	// fallback — it simply never fetches.
	f := newFakeUpstream(t, "live-model")
	ap := newUpstreamProvider(t, "acme", f)
	rt := router.New(router.StrategyAuto)
	rt.AddProvider(&router.Provider{Name: "acme", Healthy: true})
	h := &Handler{
		router:     rt,
		providers:  map[string]*activeProvider{"acme": ap},
		stopHealth: make(chan struct{}),
	}

	h.startCatalog(
		map[string][]string{"acme": {"configured-model"}},
		config.Catalog{Enabled: false, TTLSeconds: 300},
		[]string{"acme"},
	)

	if h.catalog == nil {
		t.Fatal("a disabled catalog must still be constructed")
	}
	if got := h.catalog.snapshot(); !reflect.DeepEqual(got, []string{"configured-model"}) {
		t.Errorf("snapshot = %v, want the seeded [configured-model]", got)
	}
	if f.requests() != 0 {
		t.Errorf("upstream requests = %d, want 0 — a disabled catalog must not fetch", f.requests())
	}
	if err := h.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestCatalogDisabledSeedRegistersModelsWithRouter(t *testing.T) {
	// A disabled catalog has no discovery loop, so the seed is the only id source:
	// the ids it reports must still be routable, or the endpoint would advertise
	// models that ProviderServes reports as served by nobody.
	f := newFakeUpstream(t, "live-model")
	ap := newUpstreamProvider(t, "acme", f)
	rt := router.New(router.StrategyAuto)
	rt.AddProvider(&router.Provider{Name: "acme", Healthy: true})
	h := &Handler{
		router:     rt,
		providers:  map[string]*activeProvider{"acme": ap},
		stopHealth: make(chan struct{}),
	}

	h.startCatalog(
		map[string][]string{"acme": {"configured-model"}},
		config.Catalog{Enabled: false, TTLSeconds: 300},
		[]string{"acme"},
	)

	if !rt.ProviderServes("acme", "configured-model") {
		t.Error("the router does not report the seeded id as served with the catalog disabled")
	}
	if f.requests() != 0 {
		t.Errorf("upstream requests = %d, want 0 — a disabled catalog must not fetch", f.requests())
	}
	if err := h.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestCatalogWarmLoopFetchesImmediatelyAndStops(t *testing.T) {
	f := newFakeUpstream(t, "alpha-model")
	cat, _ := buildCatalog(t, 300*time.Second, newUpstreamProvider(t, "acme", f))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		cat.run(ctx, nil)
	}()

	select {
	case <-f.fetched:
	case <-time.After(5 * time.Second):
		t.Fatal("the warm loop did not fetch on start — it must not wait a full tick first")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the warm loop did not stop on context cancellation")
	}
	if got := cat.snapshot(); !reflect.DeepEqual(got, []string{"alpha-model"}) {
		t.Errorf("snapshot = %v, want [alpha-model]", got)
	}
}

func TestCatalogWarmLoopStopsOnClosedStopChannel(t *testing.T) {
	f := newFakeUpstream(t, "alpha-model")
	cat, _ := buildCatalog(t, 300*time.Second, newUpstreamProvider(t, "acme", f))

	// Close closes this channel, and run must treat a closed stop channel exactly
	// like a cancelled context: exit without touching any provider.
	stop := make(chan struct{})
	close(stop)
	done := make(chan struct{})
	go func() {
		defer close(done)
		cat.run(context.Background(), stop)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the warm loop did not stop on the handler's stop channel")
	}
	if f.requests() != 0 {
		t.Errorf("upstream requests = %d, want 0 — a stopped loop must not fetch", f.requests())
	}
}

func TestCatalogCloseIsIdempotent(t *testing.T) {
	f := newFakeUpstream(t, "alpha-model")
	ap := newUpstreamProvider(t, "acme", f)
	rt := router.New(router.StrategyAuto)
	rt.AddProvider(&router.Provider{Name: "acme", Healthy: true})
	h := &Handler{
		router:     rt,
		providers:  map[string]*activeProvider{"acme": ap},
		stopHealth: make(chan struct{}),
	}
	h.startCatalog(nil, config.Catalog{Enabled: true, TTLSeconds: 300}, []string{"acme"})

	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// A second Close must not panic: Close closes the channel once and nils it,
	// while the warm loop only ever reads the value it captured at start.
	if err := h.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if h.catalog == nil {
		t.Error("Close must not tear the catalog down — it is the endpoint's fallback")
	}
}
