// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NEXUS contributors

package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lynuxis2026-pixel/nexus-proxy/internal/router"
)

// ─── stickyReorder (pure function) ─────────────────────────────────────────

func TestStickyReorder_MovesMatchToFront(t *testing.T) {
	a := &router.Provider{Name: "a"}
	b := &router.Provider{Name: "b"}
	c := &router.Provider{Name: "c"}
	chain := []*router.Provider{a, b, c}

	out := stickyReorder(chain, "c")

	if len(out) != 3 || out[0].Name != "c" || out[1].Name != "a" || out[2].Name != "b" {
		names := make([]string, len(out))
		for i, p := range out {
			names[i] = p.Name
		}
		t.Fatalf("got order %v, want [c a b]", names)
	}
}

func TestStickyReorder_AlreadyAtFrontIsNoop(t *testing.T) {
	a := &router.Provider{Name: "a"}
	b := &router.Provider{Name: "b"}
	chain := []*router.Provider{a, b}

	out := stickyReorder(chain, "a")

	if len(out) != 2 || out[0].Name != "a" || out[1].Name != "b" {
		t.Fatalf("expected chain unchanged when sticky is already chain[0], got %+v", out)
	}
}

func TestStickyReorder_NotFoundLeavesChainAlone(t *testing.T) {
	a := &router.Provider{Name: "a"}
	b := &router.Provider{Name: "b"}
	chain := []*router.Provider{a, b}

	out := stickyReorder(chain, "not-in-chain")

	if len(out) != 2 || out[0].Name != "a" || out[1].Name != "b" {
		t.Fatalf("sticky not present in chain should leave order alone, got %+v", out)
	}
}

func TestStickyReorder_EmptyStickyIsNoop(t *testing.T) {
	a := &router.Provider{Name: "a"}
	chain := []*router.Provider{a}

	out := stickyReorder(chain, "")

	if len(out) != 1 || out[0] != a {
		t.Fatalf("empty sticky should return chain unchanged, got %+v", out)
	}
}

// ─── Handler.setSticky / stickyProvider ────────────────────────────────────

func TestHandlerSticky_SetAndGet(t *testing.T) {
	h := &Handler{}
	if _, ok := h.stickyProvider("claude-sonnet-4-6"); ok {
		t.Fatalf("expected no sticky recorded on a fresh Handler")
	}
	h.setSticky("claude-sonnet-4-6", "provA")
	if name, ok := h.stickyProvider("claude-sonnet-4-6"); !ok || name != "provA" {
		t.Fatalf("stickyProvider = %q, %v; want provA, true", name, ok)
	}
	// Overwrite.
	h.setSticky("claude-sonnet-4-6", "provB")
	if name, _ := h.stickyProvider("claude-sonnet-4-6"); name != "provB" {
		t.Fatalf("stickyProvider after overwrite = %q, want provB", name)
	}
}

// ─── Full HandleMessages integration ───────────────────────────────────────

// TestHandleMessages_StickyProvider_StaysOnFallbackAfterKeyCools matches the
// plan's Verify scenario: two providers offering the same model; force the
// first provider's only key to cool; the second request must land on the
// second provider, and a third request (the first provider's key still
// cooling) must land on the second provider too — proving the walk stays put
// rather than bouncing.
func TestHandleMessages_StickyProvider_StaysOnFallbackAfterKeyCools(t *testing.T) {
	a := openAIServer(http.StatusOK, "from A")
	defer a.Close()
	b := openAIServer(http.StatusOK, "from B")
	defer b.Close()
	h := buildTestHandler(t, []testProv{{"A", "free", a.URL}, {"B", "free", b.URL}})

	apA := h.providers["A"]
	apA.keys = []string{"onlykey"}
	apA.cool = make([]time.Time, 1)
	apA.backoff = make([]time.Duration, 1)

	body := `{"model":"claude-haiku-4-5","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`

	// Request 1: natural chain order (A added first) with no sticky recorded yet.
	rec := doMessages(h, body)
	if got := rec.Header().Get("X-Nexus-Provider"); got != "A" {
		t.Fatalf("request 1: X-Nexus-Provider = %q, want A", got)
	}
	if name, ok := h.stickyProvider("claude-haiku-4-5"); !ok || name != "A" {
		t.Fatalf("sticky after request 1 = %q, %v; want A, true", name, ok)
	}

	// Cool A's only key — this is now its provider-level cooldown too (P2),
	// so RouteChain no longer offers A at all.
	h.coolKey(apA, "A", 0, time.Hour)
	if apA.hasAvailableKey() {
		t.Fatalf("A's only key should be cooling")
	}

	// Request 2: A unavailable → must land on B.
	rec = doMessages(h, body)
	if got := rec.Header().Get("X-Nexus-Provider"); got != "B" {
		t.Fatalf("request 2: X-Nexus-Provider = %q, want B", got)
	}
	if name, ok := h.stickyProvider("claude-haiku-4-5"); !ok || name != "B" {
		t.Fatalf("sticky after request 2 = %q, %v; want B, true", name, ok)
	}

	// Request 3: A's key still cooling → must land on B again (not bouncing).
	rec = doMessages(h, body)
	if got := rec.Header().Get("X-Nexus-Provider"); got != "B" {
		t.Fatalf("request 3: X-Nexus-Provider = %q, want B (sticky, not bouncing)", got)
	}
}

// TestHandleMessages_StickyProvider_ReordersAheadOfNaturalChainOrder proves
// stickyReorder is actually doing work inside the live handler path — not
// merely relying on the sticky provider being unavailable naturally. Both A
// and B are fully healthy (A is chain[0] by natural add-order), but with the
// sticky map already pointing at B, the request must go to B and A must not
// be hit at all.
func TestHandleMessages_StickyProvider_ReordersAheadOfNaturalChainOrder(t *testing.T) {
	var aHits int32
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&aHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []interface{}{map[string]interface{}{"index": 0, "finish_reason": "stop",
				"message": map[string]interface{}{"role": "assistant", "content": "from A"}}},
			"usage": map[string]interface{}{"prompt_tokens": 1, "completion_tokens": 1},
		})
	}))
	defer a.Close()
	b := openAIServer(http.StatusOK, "from B")
	defer b.Close()
	h := buildTestHandler(t, []testProv{{"A", "free", a.URL}, {"B", "free", b.URL}})
	h.setSticky("claude-haiku-4-5", "B")

	rec := doMessages(h, `{"model":"claude-haiku-4-5","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`)

	if got := rec.Header().Get("X-Nexus-Provider"); got != "B" {
		t.Fatalf("X-Nexus-Provider = %q, want B (sticky should reorder ahead of A, chain[0] by natural order)", got)
	}
	if got := atomic.LoadInt32(&aHits); got != 0 {
		t.Fatalf("A should not have been hit at all, got %d hits", got)
	}
}

// TestHandleMessages_StickyProvider_ScopedPerModel confirms a sticky pointer
// recorded for one requested model doesn't affect routing for a different
// requested model.
func TestHandleMessages_StickyProvider_ScopedPerModel(t *testing.T) {
	a := openAIServer(http.StatusOK, "from A")
	defer a.Close()
	b := openAIServer(http.StatusOK, "from B")
	defer b.Close()
	h := buildTestHandler(t, []testProv{{"A", "free", a.URL}, {"B", "free", b.URL}})

	// Sticky recorded for a *different* requested model than the one we're
	// about to send — must not influence this request's routing at all.
	h.setSticky("claude-sonnet-4-6", "B")

	rec := doMessages(h, `{"model":"claude-haiku-4-5","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`)

	if got := rec.Header().Get("X-Nexus-Provider"); got != "A" {
		t.Fatalf("X-Nexus-Provider = %q, want A (sticky for a different model must not apply)", got)
	}
}

// TestHandleMessages_StickyProvider_NoSnapBackWhenOriginalRecovers (P5) is the
// full round trip through the REAL P1/P2 machinery, not manual sticky/health
// pokes: provider A starts with both its keys already cooling (coolKey, as a
// real 429 would drive it), so P2 has already flipped router.Provider.Healthy
// false for A. Request 1 must land on B (A isn't even in the chain), recording
// B as sticky. A then recovers via the real P2 recovery path (recoverKey, as
// the background probe loop would call it), flipping A back to healthy. A
// SUBSEQUENT request must still prefer B — sticky must not snap back to A just
// because A is healthy again ("stay until it fails", not "always prefer the
// original chain order"). This is the scenario the plan calls out as the
// easiest to get subtly wrong, so it's verified against the live
// HandleMessages path (headers + actual upstream hit counts), not internal
// state alone.
func TestHandleMessages_StickyProvider_NoSnapBackWhenOriginalRecovers(t *testing.T) {
	var aHits int32
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&aHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []interface{}{map[string]interface{}{"index": 0, "finish_reason": "stop",
				"message": map[string]interface{}{"role": "assistant", "content": "from A"}}},
			"usage": map[string]interface{}{"prompt_tokens": 1, "completion_tokens": 1},
		})
	}))
	defer a.Close()
	b := openAIServer(http.StatusOK, "from B")
	defer b.Close()
	h := buildTestHandler(t, []testProv{{"A", "free", a.URL}, {"B", "free", b.URL}})

	apA := h.providers["A"]
	apA.keys = []string{"k1", "k2"}
	apA.cool = make([]time.Time, 2)
	apA.backoff = make([]time.Duration, 2)

	// Both of A's keys cool via the real P1/P2 path -> derives A unhealthy,
	// exactly as two real 429 responses would.
	h.coolKey(apA, "A", 0, time.Hour)
	h.coolKey(apA, "A", 1, time.Hour)
	if apA.hasAvailableKey() {
		t.Fatalf("A should have no available key")
	}
	if providerHealthy(t, h, "A") {
		t.Fatalf("A should be marked unhealthy once both keys are cooling")
	}

	body := `{"model":"claude-haiku-4-5","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`

	// Request 1: A unhealthy → RouteChain only offers B; sticky becomes B.
	rec := doMessages(h, body)
	if got := rec.Header().Get("X-Nexus-Provider"); got != "B" {
		t.Fatalf("request 1: X-Nexus-Provider = %q, want B", got)
	}
	if name, ok := h.stickyProvider("claude-haiku-4-5"); !ok || name != "B" {
		t.Fatalf("sticky after request 1 = %q, %v; want B, true", name, ok)
	}

	// A recovers via the real P2 recovery path (as the background probe loop
	// would call it after a successful probe) — flips router.Provider.Healthy
	// for A back to true.
	h.recoverKey(apA, "A", 0)
	if !providerHealthy(t, h, "A") {
		t.Fatalf("A should flip back to healthy once it has an available key again")
	}

	// Request 2: A is healthy again and, by natural chain order (added
	// first, same tier/priority as B), would be chain[0] — but sticky must
	// keep routing to B.
	rec = doMessages(h, body)
	if got := rec.Header().Get("X-Nexus-Provider"); got != "B" {
		t.Fatalf("request 2: X-Nexus-Provider = %q, want B (sticky must not snap back to recovered A)", got)
	}
	if got := atomic.LoadInt32(&aHits); got != 0 {
		t.Fatalf("A should not have been hit at all after recovering — sticky should keep it out of the picture entirely, got %d hits", got)
	}
}
