package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lynuxis2026-pixel/nexus-proxy/internal/config"
)

func TestPickKeySticky(t *testing.T) {
	a := &activeProvider{keys: []string{"a", "b", "c"}, cool: make([]time.Time, 3)}
	first, firstIdx, ok := a.pickKey()
	if !ok {
		t.Fatalf("expected ok=true with a fresh, non-cooling pool")
	}
	for i := 0; i < 5; i++ {
		k, idx, ok := a.pickKey()
		if !ok || k != first || idx != firstIdx {
			t.Fatalf("sticky pickKey should keep returning the same key while it's healthy: got %q (idx %d) ok=%v, want %q (idx %d)", k, idx, ok, first, firstIdx)
		}
	}
}

func TestPickKeySkipsCooling(t *testing.T) {
	a := &activeProvider{keys: []string{"a", "b"}, cool: make([]time.Time, 2)}
	a.penalize(0, time.Hour) // "a" is cooling
	for i := 0; i < 5; i++ {
		k, idx, ok := a.pickKey()
		if !ok || k == "a" || idx == 0 {
			t.Fatalf("cooling key must be skipped, got %q (idx %d) ok=%v", k, idx, ok)
		}
	}
}

func TestPickKeyAllCoolingReturnsNotOK(t *testing.T) {
	a := &activeProvider{keys: []string{"a", "b"}, cool: make([]time.Time, 2)}
	a.penalize(0, time.Hour)
	a.penalize(1, time.Hour)
	if k, idx, ok := a.pickKey(); ok {
		t.Fatalf("expected ok=false when every key is cooling, got %q (idx %d)", k, idx)
	}
	if a.hasAvailableKey() {
		t.Fatalf("hasAvailableKey should be false when every key is cooling")
	}
}

func TestPickKeyFallbackToApiKey(t *testing.T) {
	a := &activeProvider{apiKey: "solo"}
	if k, idx, ok := a.pickKey(); k != "solo" || idx != -1 || !ok {
		t.Fatalf("no pool should fall back to apiKey, got %q %d ok=%v", k, idx, ok)
	}
	if !a.hasAvailableKey() {
		t.Fatalf("no pool (fallback-to-apiKey) should always report available")
	}
}

func TestResolveProviderKeys(t *testing.T) {
	if ks := resolveProviderKeys(config.Provider{APIKeys: []string{"a", "b"}, APIKey: "ignored"}); len(ks) != 2 || ks[0] != "a" {
		t.Fatalf("api_keys should win: %v", ks)
	}
	if ks := resolveProviderKeys(config.Provider{APIKey: "solo"}); len(ks) != 1 || ks[0] != "solo" {
		t.Fatalf("single key: %v", ks)
	}
	t.Setenv("NEXUS_TEST_KEY", "envval")
	if ks := resolveProviderKeys(config.Provider{APIKey: "env:NEXUS_TEST_KEY"}); ks[0] != "envval" {
		t.Fatalf("env resolution: %v", ks)
	}
}

func TestKeyRotationOn429(t *testing.T) {
	var badHits, goodHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer good" {
			atomic.AddInt32(&goodHits, 1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id": "x", "object": "chat.completion", "model": "m",
				"choices": []interface{}{map[string]interface{}{"index": 0, "finish_reason": "stop",
					"message": map[string]interface{}{"role": "assistant", "content": "ok"}}},
				"usage": map[string]interface{}{"prompt_tokens": 5, "completion_tokens": 2},
			})
			return
		}
		atomic.AddInt32(&badHits, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer srv.Close()

	h := buildTestHandler(t, []testProv{{"p", "free", srv.URL}})
	ap := h.providers["p"]
	ap.keys = []string{"bad", "good"}
	ap.cool = make([]time.Time, 2)
	ap.rr = 0 // sticky current index — force the first pick to be index 0 ("bad")

	rec := doMessages(h, `{"model":"claude-haiku-4-5","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if badHits < 1 || goodHits != 1 {
		t.Fatalf("bad=%d good=%d; expected a 429 on the bad key then rotation to the good key", badHits, goodHits)
	}
}

// providerHealthy looks up a provider's current router.Provider.Healthy flag.
func providerHealthy(t *testing.T, h *Handler, name string) bool {
	t.Helper()
	for _, p := range h.router.Providers() {
		if p.Name == name {
			return p.Healthy
		}
	}
	t.Fatalf("provider %q not registered in router", name)
	return false
}

// TestProbeCoolingKeys_RecoversAndFlipsHealthy exercises one probeCoolingKeys
// pass directly (no ticker): a single-key provider is cooling (its last —
// and only — available key), so the provider is already marked unhealthy.
// The probe server answers 200, so the pass should clear the key's cooldown
// and flip the provider back to healthy.
func TestProbeCoolingKeys_RecoversAndFlipsHealthy(t *testing.T) {
	srv := openAIServer(http.StatusOK, "pong")
	defer srv.Close()

	h := buildTestHandler(t, []testProv{{"p", "free", srv.URL}})
	ap := h.providers["p"]
	ap.keys = []string{"onlykey"}
	ap.cool = make([]time.Time, 1)
	ap.backoff = make([]time.Duration, 1)

	// Cool the provider's only key exactly like a real 429 would, via the
	// same Handler method callUpstream uses — this also drives SetHealthy
	// false since it was the provider's last available key.
	h.coolKey(ap, "p", 0, initialKeyBackoff)
	if ap.hasAvailableKey() {
		t.Fatalf("key should be cooling right after coolKey")
	}
	if providerHealthy(t, h, "p") {
		t.Fatalf("provider should be marked unhealthy once its last key started cooling")
	}
	// Backdate the deadline so it's due for a probe on this pass (coolKey put
	// it initialKeyBackoff in the future, which a real ticker wouldn't reach
	// for another 10s).
	ap.cool[0] = time.Now().Add(-time.Millisecond)

	h.probeCoolingKeys()

	if !ap.hasAvailableKey() {
		t.Fatalf("a non-429 probe response should clear the key's cooldown")
	}
	if ap.backoff[0] != 0 {
		t.Fatalf("backoff should reset to 0 on recovery, got %v", ap.backoff[0])
	}
	if !providerHealthy(t, h, "p") {
		t.Fatalf("provider should flip back to healthy once its only key recovers")
	}
}

// TestProbeCoolingKeys_SkipsNotYetDue confirms a cooling key whose backoff
// deadline hasn't passed yet is left alone by the probe pass (no request
// sent, cooldown untouched) — probes are gated by each key's own deadline,
// not fired on every pass.
func TestProbeCoolingKeys_SkipsNotYetDue(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := buildTestHandler(t, []testProv{{"p", "free", srv.URL}})
	ap := h.providers["p"]
	ap.keys = []string{"onlykey"}
	ap.cool = make([]time.Time, 1)
	ap.backoff = make([]time.Duration, 1)
	h.coolKey(ap, "p", 0, initialKeyBackoff) // deadline is initialKeyBackoff in the future — not due

	h.probeCoolingKeys()

	if hits != 0 {
		t.Fatalf("expected no probe request for a not-yet-due key, got %d", hits)
	}
	if ap.hasAvailableKey() {
		t.Fatalf("key should still be cooling, untouched by the skipped pass")
	}
}

// TestProbeCoolingKeys_BackoffDoublesAndCaps exercises the probe's own repeat-
// 429 handling directly: each pass against an always-429 server should double
// the key's backoff, capped at maxKeyBackoff.
func TestProbeCoolingKeys_BackoffDoublesAndCaps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"still rate limited"}}`))
	}))
	defer srv.Close()

	h := buildTestHandler(t, []testProv{{"p", "free", srv.URL}})
	ap := h.providers["p"]
	ap.keys = []string{"onlykey"}
	ap.cool = make([]time.Time, 1)
	ap.backoff = make([]time.Duration, 1)
	h.coolKey(ap, "p", 0, initialKeyBackoff) // backoff[0] = 10s

	wantSequence := []time.Duration{
		20 * time.Second,
		40 * time.Second,
		80 * time.Second,
		160 * time.Second,
		maxKeyBackoff, // 320s would exceed the 5m cap
		maxKeyBackoff,
	}
	for i, want := range wantSequence {
		ap.cool[0] = time.Now().Add(-time.Millisecond) // force due
		h.probeCoolingKeys()
		if ap.backoff[0] != want {
			t.Fatalf("pass %d: backoff = %v, want %v", i, ap.backoff[0], want)
		}
		if ap.hasAvailableKey() {
			t.Fatalf("pass %d: key should still be cooling after a repeat 429", i)
		}
	}
	if providerHealthy(t, h, "p") {
		t.Fatalf("provider should remain unhealthy while its only key keeps 429ing")
	}
}

// ─── P5: integration tests across P1-P4 ────────────────────────────────────

// TestKeyStickyAcrossRequests_NoSnapBackAfterRecovery proves stickiness at the
// key level (P1) survives ACROSS separate HandleMessages calls, not just
// within callUpstream's own single-request retry loop (already covered by
// TestKeyRotationOn429): a two-key provider where keyA 429s on request 1,
// causing an in-request rotation to keyB. Even after keyA's cooldown is
// cleared (simulating it naturally recovering), a later, separate request
// must still be served by keyB — pickKey only re-scans away from its current
// "rr" pointer when THAT key is cooling, it never proactively switches back
// to an earlier key just because that key became available again.
func TestKeyStickyAcrossRequests_NoSnapBackAfterRecovery(t *testing.T) {
	var mu sync.Mutex
	var keysUsed []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		keysUsed = append(keysUsed, key)
		mu.Unlock()
		if key == "keyA" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id": "x", "object": "chat.completion", "model": "m",
			"choices": []interface{}{map[string]interface{}{"index": 0, "finish_reason": "stop",
				"message": map[string]interface{}{"role": "assistant", "content": "ok"}}},
			"usage": map[string]interface{}{"prompt_tokens": 5, "completion_tokens": 2},
		})
	}))
	defer srv.Close()

	h := buildTestHandler(t, []testProv{{"p", "free", srv.URL}})
	ap := h.providers["p"]
	ap.keys = []string{"keyA", "keyB"}
	ap.cool = make([]time.Time, 2)
	ap.backoff = make([]time.Duration, 2)
	ap.rr = 0 // force the first pick to be keyA

	body := `{"model":"claude-haiku-4-5","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`

	// Request 1: keyA 429s -> in-request rotation (P1) to keyB -> succeeds.
	rec := doMessages(h, body)
	if rec.Code != 200 {
		t.Fatalf("request 1: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// Simulate keyA recovering on its own (its cooldown just happened to
	// elapse) — this must NOT cause pickKey to bounce back to it, since keyB
	// is now the sticky "current" key (a.rr).
	ap.clearCooldown(0)
	if !ap.hasAvailableKey() {
		t.Fatalf("keyA should read as available again after clearCooldown")
	}

	mu.Lock()
	keysUsed = nil
	mu.Unlock()

	// Request 2: both keys now read as available by cooldown, but pickKey
	// must stick to keyB, not fall back to keyA.
	rec = doMessages(h, body)
	if rec.Code != 200 {
		t.Fatalf("request 2: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	used := append([]string(nil), keysUsed...)
	mu.Unlock()
	if len(used) != 1 || used[0] != "keyB" {
		t.Fatalf("request 2 should be served by sticky keyB only (not bouncing back to recovered keyA), got %v", used)
	}
}

// TestProbeCoolingKeys_RecoversOnNon429NonSuccessStatus confirms the recovery
// signal the probe loop (P2) uses is genuinely "non-429", per settled
// requirement #6 — not narrowed to "2xx only" anywhere. A cooling key whose
// probe response comes back 500 (non-429, but also not a success status) must
// still be treated as recovered, exactly like
// TestProbeCoolingKeys_RecoversAndFlipsHealthy's 200 case above.
func TestProbeCoolingKeys_RecoversOnNon429NonSuccessStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // non-429, but also not 2xx
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer srv.Close()

	h := buildTestHandler(t, []testProv{{"p", "free", srv.URL}})
	ap := h.providers["p"]
	ap.keys = []string{"onlykey"}
	ap.cool = make([]time.Time, 1)
	ap.backoff = make([]time.Duration, 1)

	h.coolKey(ap, "p", 0, initialKeyBackoff)
	if providerHealthy(t, h, "p") {
		t.Fatalf("provider should be unhealthy once its only key is cooling")
	}
	ap.cool[0] = time.Now().Add(-time.Millisecond) // force due

	h.probeCoolingKeys()

	if !ap.hasAvailableKey() {
		t.Fatalf("a 500 (non-429) probe response should still clear the key's cooldown — recovery is non-429, not 2xx-only")
	}
	if ap.backoff[0] != 0 {
		t.Fatalf("backoff should reset to 0 on recovery, got %v", ap.backoff[0])
	}
	if !providerHealthy(t, h, "p") {
		t.Fatalf("provider should flip back to healthy once its only key recovers")
	}
}
