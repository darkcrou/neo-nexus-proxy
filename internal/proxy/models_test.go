// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NEXUS contributors

package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lynuxis2026-pixel/nexus-proxy/internal/router"
)

// ─── test fixtures ──────────────────────────────────────────────────────────

// fakeModelProvider is one fake upstream playing both roles these tests need: it
// lists its own ids on /models (what discovery reads) and answers
// /chat/completions only for those ids — any other requested model is a 404,
// exactly as a real provider that does not offer the id would answer. Chat
// requests and listings are counted separately, so "the request reached this
// provider" can never be confused with "discovery asked it for its models".
type fakeModelProvider struct {
	srv    *httptest.Server
	ids    []string
	chat   atomic.Int64
	listed atomic.Int64
	// listStatus, when non-zero, is answered by /models instead of the id list: the
	// provider is up, its listing endpoint is not.
	listStatus int
	chatStatus func(*http.Request, string) int
}

func newFakeModelProvider(t *testing.T, ids ...string) *fakeModelProvider {
	t.Helper()
	return startFakeModelProvider(t, &fakeModelProvider{ids: ids})
}

// newUnlistableModelProvider is a fake whose /models always answers 500 while chat
// keeps working, so a failed listing can be told apart from an unreachable provider.
func newUnlistableModelProvider(t *testing.T) *fakeModelProvider {
	t.Helper()
	return startFakeModelProvider(t, &fakeModelProvider{listStatus: http.StatusInternalServerError})
}

func startFakeModelProvider(t *testing.T, f *fakeModelProvider) *fakeModelProvider {
	t.Helper()
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			f.listed.Add(1)
			if f.listStatus != 0 {
				w.WriteHeader(f.listStatus)
				return
			}
			data := make([]map[string]string, 0, len(f.ids))
			for _, id := range f.ids {
				data = append(data, map[string]string{"id": id, "object": "model"})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"object": "list", "data": data})
			return
		}

		f.chat.Add(1)
		var req map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&req)
		model, _ := req["model"].(string)
		if !f.serves(model) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"model not found"}}`))
			return
		}
		if f.chatStatus != nil {
			if status := f.chatStatus(r, model); status != http.StatusOK {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":{"message":"upstream unavailable"}}`))
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id": "x", "object": "chat.completion", "model": model,
			"choices": []interface{}{map[string]interface{}{"index": 0, "finish_reason": "stop",
				"message": map[string]interface{}{"role": "assistant", "content": "from " + model}}},
			"usage": map[string]interface{}{"prompt_tokens": 3, "completion_tokens": 2},
		})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeModelProvider) serves(model string) bool {
	for _, id := range f.ids {
		if id == model {
			return true
		}
	}
	return false
}

// modelsProvider is one provider entry in the config these tests build.
type modelsProvider struct {
	name   string
	url    string
	tier   string
	models []string // empty → the entry declares no models, so discovery alone decides
}

// newModelsHandler builds a real Handler over fake providers with the catalog's
// warm loop switched off. Determinism is the point: the endpoint's answer and the
// routability assertions must see one and the same id set, and a running warm loop
// could replace it between two of them. Discovery is instead driven once, by hand,
// before any assertion — see discover.
func newModelsHandler(t *testing.T, strategy string, provs ...modelsProvider) *Handler {
	t.Helper()
	var b strings.Builder
	b.WriteString("[catalog]\nenabled = false\n\n[routing]\n")
	fmt.Fprintf(&b, "strategy = %q\n\n", strategy)
	for _, p := range provs {
		fmt.Fprintf(&b, "[[providers]]\nname = %q\ntype = \"openai-compatible\"\nbase_url = %q\napi_key = \"sk-test\"\ntier = %q\n",
			p.name, p.url, p.tier)
		if len(p.models) > 0 {
			quoted := make([]string, len(p.models))
			for i, id := range p.models {
				quoted[i] = fmt.Sprintf("%q", id)
			}
			fmt.Fprintf(&b, "models = [%s]\n", strings.Join(quoted, ", "))
		}
		b.WriteString("\n")
	}
	return newHandlerFromConfig(t, b.String())
}

// discover runs one discovery pass by hand. The warm loop is off in these tests,
// so this is the only fetch, and it completes before the first assertion.
func discover(t *testing.T, h *Handler) {
	t.Helper()
	if h.catalog == nil {
		t.Fatal("handler has no catalog to discover with")
	}
	h.catalog.refreshAll(context.Background())
}

// modelsList calls GET /v1/models and returns the ids in wire order plus the raw
// entry objects, so shape assertions read the JSON a client actually receives.
func modelsList(t *testing.T, h *Handler) ([]string, []map[string]json.RawMessage) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.HandleModels(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/models = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Object string                       `json:"object"`
		Data   []map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("not an OpenAI list: %v\n%s", err, rec.Body.String())
	}
	if resp.Object != "list" {
		t.Errorf("object = %q, want list", resp.Object)
	}
	ids := make([]string, 0, len(resp.Data))
	for _, e := range resp.Data {
		var id string
		if err := json.Unmarshal(e["id"], &id); err != nil {
			t.Fatalf("entry carries no string id: %v (%v)", err, e)
		}
		ids = append(ids, id)
	}
	return ids, resp.Data
}

// ─── endpoint contract ──────────────────────────────────────────────────────

func TestModels_ReturnsTheUnionOfDiscoveredModelsInCatalogOrder(t *testing.T) {
	// Registration order is the reverse of both the alphabetical and the
	// sorted-by-id order, and neither list is internally sorted, so sorting or map
	// iteration anywhere in the path would show up here.
	second := newFakeModelProvider(t, "zulu")
	first := newFakeModelProvider(t, "mike")
	h := newModelsHandler(t, "auto",
		modelsProvider{name: "second", url: second.srv.URL, tier: "free"},
		modelsProvider{name: "first", url: first.srv.URL, tier: "free"},
	)
	discover(t, h)

	ids, _ := modelsList(t, h)
	if want := []string{"zulu", "mike"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("ids = %v, want %v (registration order, then each provider's own order)", ids, want)
	}
}

func TestModels_ListsASharedModelIDExactlyOnce(t *testing.T) {
	first := newFakeModelProvider(t, "shared-model", "first-only")
	second := newFakeModelProvider(t, "second-only", "shared-model")
	h := newModelsHandler(t, "auto",
		modelsProvider{name: "first", url: first.srv.URL, tier: "free"},
		modelsProvider{name: "second", url: second.srv.URL, tier: "free"},
	)
	discover(t, h)

	ids, _ := modelsList(t, h)
	if want := []string{"shared-model", "first-only", "second-only"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("ids = %v, want %v", ids, want)
	}
	seen := 0
	for _, id := range ids {
		if id == "shared-model" {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("shared-model appears %d times, want exactly once", seen)
	}
}

func TestModels_EntriesAreNexusOwnedAndCarryNoCreatedTimestamp(t *testing.T) {
	// D-10: no fabricated metadata. The entry names a capability, not a destination,
	// so it is owned by nexus; and there is no honest creation time to report, so the
	// field is omitted rather than invented.
	f := newFakeModelProvider(t, "some-model")
	h := newModelsHandler(t, "auto", modelsProvider{name: "acme", url: f.srv.URL, tier: "free"})
	discover(t, h)

	_, entries := modelsList(t, h)
	if len(entries) == 0 {
		t.Fatal("no model entries to inspect")
	}
	for _, e := range entries {
		if _, ok := e["created"]; ok {
			t.Errorf("entry %v carries a created field, want none", e)
		}
		if len(e) != 3 {
			t.Errorf("entry has fields %v, want exactly id, object, owned_by", e)
		}
		var object, ownedBy string
		_ = json.Unmarshal(e["object"], &object)
		_ = json.Unmarshal(e["owned_by"], &ownedBy)
		if object != "model" {
			t.Errorf("object = %q, want model", object)
		}
		if ownedBy != "nexus" {
			t.Errorf("owned_by = %q, want nexus", ownedBy)
		}
	}
}

func TestModels_FallsBackToTheStubClaudeIDsWhenNothingIsDiscovered(t *testing.T) {
	// A provider family with no default list and no configured models, with warming
	// off: every rung above the stub is empty, and the client must still never see an
	// empty list.
	f := newFakeModelProvider(t, "never-discovered")
	h := newModelsHandler(t, "auto", modelsProvider{name: "custom", url: f.srv.URL, tier: "standard"})

	ids, _ := modelsList(t, h)
	want := []string{"claude-opus-4-5", "claude-sonnet-4-6", "claude-haiku-4-5"}
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("ids = %v, want the stub fallback %v", ids, want)
	}
}

func TestModels_FailedListingFallsThroughToTheStubList(t *testing.T) {
	// The plan's ladder criterion: a provider whose listing fails must not take the
	// endpoint down with it. Warming is off and the entry declares no models, so the
	// seed is empty too and the stub is the only rung left — but the listing has to
	// have been attempted, or this test would pass by never running discovery at all.
	f := newUnlistableModelProvider(t)
	h := newModelsHandler(t, "auto", modelsProvider{name: "custom", url: f.srv.URL, tier: "standard"})

	discover(t, h)

	if got := f.listed.Load(); got != 1 {
		t.Fatalf("listing requests = %d, want 1 — discovery must have run and failed", got)
	}
	ids, _ := modelsList(t, h)
	want := []string{"claude-opus-4-5", "claude-sonnet-4-6", "claude-haiku-4-5"}
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("ids = %v, want the stub fallback %v — a failed listing must not empty the list", ids, want)
	}
}

func TestModels_ServesFromCacheWithoutContactingAnyProvider(t *testing.T) {
	// The endpoint carries no authentication, so a request must not be able to force
	// a keyed upstream call (D-8). It answers from the catalog and nothing else.
	f := newFakeModelProvider(t, "discovered-model")
	h := newModelsHandler(t, "auto", modelsProvider{name: "acme", url: f.srv.URL, tier: "free"})
	discover(t, h)

	before := f.listed.Load()
	ids, _ := modelsList(t, h)

	// The discovered id must be what comes back: an empty cache would fall through to
	// the stub Claude list and still leave the counter untouched.
	if want := []string{"discovered-model"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("ids = %v, want the discovered %v — the endpoint must serve the cached snapshot", ids, want)
	}
	if got := f.listed.Load(); got != before {
		t.Errorf("listing requests = %d after GET /v1/models, want %d — the endpoint must never trigger discovery", got, before)
	}
}

// ─── routability invariant ──────────────────────────────────────────────────

var allComplexities = []router.Complexity{
	router.ComplexitySimple,
	router.ComplexityStandard,
	router.ComplexityComplex,
	router.ComplexityCritical,
}

// allStrategies is every routing strategy RouteChain can be configured with, so the
// routability guard proves the partition holds under each of them rather than the
// two that happen to be tested by hand.
var allStrategies = []string{"auto", "direct", "cheapest", "fastest", "manual"}

func chainNames(chain []*router.Provider) []string {
	out := make([]string, len(chain))
	for i, p := range chain {
		out[i] = p.Name
	}
	return out
}

func TestModels_EveryAdvertisedModelIsRoutableUnderEveryStrategy(t *testing.T) {
	// The regression guard for the whole feature: an id the endpoint advertises must
	// reach a provider that serves it, or the client sees a 404 the proxy invented.
	// The head is checked through Router.ProviderServes — never by reading a
	// Provider's model map — so the assertion stays race-free against discovery.
	for _, strategy := range allStrategies {
		t.Run(strategy, func(t *testing.T) {
			free := newFakeModelProvider(t, "free-only", "shared-model")
			premium := newFakeModelProvider(t, "premium-only", "shared-model")
			h := newModelsHandler(t, strategy,
				modelsProvider{name: "free", url: free.srv.URL, tier: "free"},
				modelsProvider{name: "premium", url: premium.srv.URL, tier: "premium"},
			)
			discover(t, h)

			ids, _ := modelsList(t, h)
			if len(ids) != 3 {
				t.Fatalf("ids = %v, want the three discovered ids", ids)
			}
			for _, id := range ids {
				for _, c := range allComplexities {
					chain := h.router.RouteChain(id, c)
					if len(chain) == 0 {
						t.Errorf("%q/%s: empty chain", id, c)
						continue
					}
					if !h.router.ProviderServes(chain[0].Name, id) {
						t.Errorf("%q/%s: chain head %q does not serve the id (chain %v)",
							id, c, chain[0].Name, chainNames(chain))
					}
				}
			}
		})
	}
}

func TestCascade_SkipsACandidateThatDoesNotServeTheRequestedID(t *testing.T) {
	// CascadeChain is an independent chain source (it calls autoChain/healthy
	// directly), so the routability guard on RouteChain does not cover it: without the
	// partition re-applied to the cascade chain, the cheap provider is tried first and
	// can only answer 404 for an id it does not serve.
	cheap := newFakeModelProvider(t, "cheap-model")
	premium := newFakeModelProvider(t, "premium-model")
	h := newModelsHandler(t, "auto",
		modelsProvider{name: "cheap", url: cheap.srv.URL, tier: "free"},
		modelsProvider{name: "premium", url: premium.srv.URL, tier: "premium"},
	)
	discover(t, h)
	h.cascade = true

	rec := doMessages(h, `{"model":"premium-model","max_tokens":100,"messages":[{"role":"user","content":"refactor this"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Nexus-Provider"); got != "premium" {
		t.Errorf("X-Nexus-Provider = %q, want premium — the only provider that serves the id", got)
	}
	if got := cheap.chat.Load(); got != 0 {
		t.Errorf("cheap handled %d requests, want 0 — a cascade candidate that cannot serve the id must not be tried", got)
	}
	if got := premium.chat.Load(); got != 1 {
		t.Errorf("premium handled %d requests, want 1", got)
	}
}

func TestModels_StickyProviderThatDoesNotServeTheModelIsExcluded(t *testing.T) {
	a := newFakeModelProvider(t, "a-model")
	b := newFakeModelProvider(t, "b-model")
	h := newModelsHandler(t, "auto",
		modelsProvider{name: "a", url: a.srv.URL, tier: "free", models: []string{"a-model"}},
		modelsProvider{name: "b", url: b.srv.URL, tier: "premium", models: []string{"b-model"}},
	)
	h.setSticky("b-model", "a")

	rec := doMessages(h, `{"model":"b-model","max_tokens":20,"messages":[{"role":"user","content":"hi"}]}`)

	if got := rec.Header().Get("X-Nexus-Provider"); got != "b" {
		t.Errorf("X-Nexus-Provider = %q, want b, the only provider that serves the id", got)
	}
	if got := a.chat.Load(); got != 0 {
		t.Errorf("a handled %d requests, want 0 — it does not serve the requested id", got)
	}
}

func doModelRequest(h *Handler, path, model string) *httptest.ResponseRecorder {
	body := fmt.Sprintf(`{"model":%q,"max_tokens":20,"messages":[{"role":"user","content":"hi"}]}`, model)
	if path == "/v1/messages" {
		return doMessages(h, body)
	}
	rec := httptest.NewRecorder()
	h.HandleChatCompletions(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	return rec
}

func TestModels_BillingFailoverSticksPerModelAcrossEndpointsAndRecovery(t *testing.T) {
	var limited atomic.Bool
	limited.Store(true)
	a := startFakeModelProvider(t, &fakeModelProvider{
		ids: []string{"shared", "other"},
		chatStatus: func(_ *http.Request, model string) int {
			if model == "shared" && limited.Load() {
				return http.StatusPaymentRequired
			}
			return http.StatusOK
		},
	})
	b := newFakeModelProvider(t, "shared", "other")
	h := newModelsHandler(t, "direct",
		modelsProvider{name: "a", url: a.srv.URL, tier: "standard"},
		modelsProvider{name: "b", url: b.srv.URL, tier: "standard"},
	)
	discover(t, h)
	assertProvider := func(path, model, want string) {
		t.Helper()
		rec := doModelRequest(h, path, model)
		if got := rec.Header().Get("X-Nexus-Provider"); rec.Code != http.StatusOK || got != want {
			t.Fatalf("%s %s: status/provider = %d/%q, want 200/%s. Body: %s", path, model, rec.Code, got, want, rec.Body)
		}
	}
	assertProvider("/v1/chat/completions", "shared", "b")
	if providerHealthy(t, h, "a") {
		t.Fatal("402 on the last key should cool the provider globally")
	}

	ap := h.providers["a"]
	expireCooldown := func() {
		ap.mu.Lock()
		defer ap.mu.Unlock()
		ap.cool[0] = time.Now().Add(-time.Second)
	}
	expireCooldown()
	if ap.hasAvailableKey() {
		t.Fatal("expiry should make the key eligible for a probe")
	}
	h.probeCoolingKeys()
	if providerHealthy(t, h, "a") || a.chat.Load() != 2 {
		t.Fatal("first recovery probe should attempt the limited model and keep the provider cooling")
	}
	limited.Store(false)
	expireCooldown()
	h.probeCoolingKeys()
	if !providerHealthy(t, h, "a") || a.chat.Load() != 3 {
		t.Fatal("successful probe should restore the provider to all model routes")
	}

	assertProvider("/v1/messages", "shared", "b")
	assertProvider("/v1/chat/completions", "shared", "b")
	assertProvider("/v1/chat/completions", "other", "a")
	if got := a.chat.Load(); got != 4 {
		t.Errorf("provider a handled %d requests, want the original request, two probes, and other model", got)
	}
}

func TestModels_RejectsUnlistedIDsAndSkipsNonServingProviders(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			owner := startFakeModelProvider(t, &fakeModelProvider{
				ids:        []string{"known"},
				chatStatus: func(*http.Request, string) int { return http.StatusTooManyRequests },
			})
			unrelated := newFakeModelProvider(t, "unrelated")
			h := newModelsHandler(t, "direct",
				modelsProvider{name: "owner", url: owner.srv.URL, tier: "standard"},
				modelsProvider{name: "unrelated", url: unrelated.srv.URL, tier: "standard"},
			)
			discover(t, h)
			for _, want := range []struct {
				model string
				code  int
			}{
				{"missing", http.StatusNotFound},
				{"known", http.StatusTooManyRequests},
				{"known", http.StatusServiceUnavailable},
			} {
				rec := doModelRequest(h, path, want.model)
				if rec.Code != want.code {
					t.Fatalf("%s: status = %d, want %d. Body: %s", want.model, rec.Code, want.code, rec.Body)
				}
			}
			if owner.chat.Load() != 1 || unrelated.chat.Load() != 0 {
				t.Fatalf("upstream hits = %d/%d, want 1/0", owner.chat.Load(), unrelated.chat.Load())
			}
		})
	}
}

func TestModels_AutoForwardsDiscoveredIDWithoutApplyingDefaultMapping(t *testing.T) {
	upstream := newFakeModelProvider(t, "default-model", "requested-model")
	h := newModelsHandler(t, "auto", modelsProvider{
		name: "provider", url: upstream.srv.URL, tier: "standard", models: []string{"default-model"},
	})
	discover(t, h)
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		rec := doModelRequest(h, path, "requested-model")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "from requested-model") {
			t.Fatalf("%s selected the wrong upstream model: %d %s", path, rec.Code, rec.Body)
		}
	}
}

func TestModels_ProbeRecoversAProviderAfterItsLimitedModelDisappears(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusPaymentRequired)
	upstream := startFakeModelProvider(t, &fakeModelProvider{
		ids: []string{"retired", "current"},
		chatStatus: func(_ *http.Request, model string) int {
			if model == "retired" {
				return int(status.Load())
			}
			return http.StatusOK
		},
	})
	h := newModelsHandler(t, "direct", modelsProvider{name: "provider", url: upstream.srv.URL, tier: "standard"})
	discover(t, h)
	if rec := doModelRequest(h, "/v1/chat/completions", "retired"); rec.Code != http.StatusPaymentRequired {
		t.Fatalf("initial request = %d, want 402", rec.Code)
	}
	status.Store(http.StatusNotFound)
	ap := h.providers["provider"]
	ap.mu.Lock()
	ap.cool[0] = time.Now().Add(-time.Second)
	ap.mu.Unlock()
	h.probeCoolingKeys()
	if !providerHealthy(t, h, "provider") || upstream.chat.Load() != 3 {
		t.Fatal("the retired model should fail its probe and the provider should recover through the alternate model")
	}
}

func TestModels_FailsOverWhenAListedModelDisappearsFromOneProvider(t *testing.T) {
	a := startFakeModelProvider(t, &fakeModelProvider{
		ids:        []string{"shared", "other"},
		chatStatus: func(*http.Request, string) int { return http.StatusNotFound },
	})
	b := newFakeModelProvider(t, "shared")
	h := newModelsHandler(t, "direct",
		modelsProvider{name: "a", url: a.srv.URL, tier: "standard"},
		modelsProvider{name: "b", url: b.srv.URL, tier: "standard"},
	)
	discover(t, h)
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		h.setSticky("shared", "a")
		rec := doModelRequest(h, path, "shared")
		if got := rec.Header().Get("X-Nexus-Provider"); rec.Code != http.StatusOK || got != "b" {
			t.Fatalf("%s: status/provider = %d/%q, want 200/b", path, rec.Code, got)
		}
	}
	if !providerHealthy(t, h, "a") || a.chat.Load() != 2 {
		t.Fatal("the missing model should fail over without cooling the provider")
	}
}

func TestModels_DiscoveredModelReachesTheProviderThatServesIt(t *testing.T) {
	// End to end: neither provider declares models, so the ids the endpoint
	// advertises are exactly the discovered ones, and each fake answers 200 only for
	// its own id. A mis-ordered chain would surface as a 404, not as a silent reroute.
	for _, strategy := range []string{"auto", "direct"} {
		t.Run(strategy, func(t *testing.T) {
			alpha := newFakeModelProvider(t, "alpha-1")
			beta := newFakeModelProvider(t, "beta-1")
			h := newModelsHandler(t, strategy,
				modelsProvider{name: "alpha", url: alpha.srv.URL, tier: "free"},
				modelsProvider{name: "beta", url: beta.srv.URL, tier: "premium"},
			)
			discover(t, h)

			rec := doMessages(h, `{"model":"beta-1","max_tokens":20,"messages":[{"role":"user","content":"hi"}]}`)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("X-Nexus-Provider"); got != "beta" {
				t.Errorf("X-Nexus-Provider = %q, want beta", got)
			}
			if got := alpha.chat.Load(); got != 0 {
				t.Errorf("alpha handled %d requests, want 0 — it does not serve the requested id", got)
			}
			if got := beta.chat.Load(); got != 1 {
				t.Errorf("beta handled %d requests, want 1", got)
			}
		})
	}
}
