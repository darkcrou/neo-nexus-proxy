package proxy

// U4: per-upstream-attempt usage-event recording. These tests exercise the
// full path — callUpstream → relay → logResult → storage usage_events —
// through the real handler (openai-compatible providers via httptest), and
// the Anthropic-format relays through direct calls (the openai_test.go
// convention), since the built-in Anthropic provider has a fixed BaseURL.
//
// Design under test (see docs/tickets/usage-measurement.md U4):
//   - every upstream attempt that produced a response or a transport error
//     gets exactly one immutable event; nothing is double-recorded
//   - key-rotation 429s, retry/failover discards, cascade escalations and
//     cooldown probes record events the client never sees
//   - token fields come from the raw (presence-preserving) parsers, never
//     the normalized cost arithmetic
//   - quota headers are transcribed once (callUpstream) and flow to the
//     event via attemptInfo; NEXUS never computes utilization itself
//   - events with no DB configured are silently skipped

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lynuxis2026-pixel/nexus-proxy/internal/providers"
	"github.com/lynuxis2026-pixel/nexus-proxy/internal/router"
	"github.com/lynuxis2026-pixel/nexus-proxy/internal/storage"
)

// buildUsageHandler wires buildTestHandler plus a real SQLite DB so usage
// events land in storage, exactly like production.
func buildUsageHandler(t *testing.T, provs []testProv) (*Handler, *storage.DB) {
	t.Helper()
	h := buildTestHandler(t, provs)
	db, err := storage.New(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("open usage db: %v", err)
	}
	h.db = db
	t.Cleanup(func() { _ = db.Close() })
	return h, db
}

// fetchEvents returns all recorded events, oldest first.
func fetchEvents(t *testing.T, db *storage.DB) []*storage.UsageEvent {
	t.Helper()
	evs, err := db.GetUsageEvents(storage.UsageFilter{Ascending: true})
	if err != nil {
		t.Fatalf("GetUsageEvents: %v", err)
	}
	return evs
}

func requireEvents(t *testing.T, db *storage.DB, want int) []*storage.UsageEvent {
	t.Helper()
	evs := fetchEvents(t, db)
	if len(evs) != want {
		t.Fatalf("got %d usage events, want %d:\n%s", len(evs), want, dumpEvents(evs))
	}
	return evs
}

func dumpEvents(evs []*storage.UsageEvent) string {
	var b strings.Builder
	for _, e := range evs {
		fmt.Fprintf(&b, "  id=%d provider=%s status=%d attempt=%d key=%v probe=%v in=%v out=%v err=%q\n",
			e.ID, e.Provider, e.Status, e.Attempt, e.KeyIndex, e.Probe, e.In, e.Out, e.Error)
	}
	return b.String()
}

func wantIntPtr(t *testing.T, got *int, want int, field string) {
	t.Helper()
	if got == nil {
		t.Errorf("%s = nil, want %d", field, want)
		return
	}
	if *got != want {
		t.Errorf("%s = %d, want %d", field, *got, want)
	}
}

func wantNilIntPtr(t *testing.T, got *int, field string) {
	t.Helper()
	if got != nil {
		t.Errorf("%s = %d, want nil", field, *got)
	}
}

// ─── end-to-end through HandleMessages (openai-compatible) ──────────────────

// oaiServerWithUsage is an openai-compatible upstream with full usage detail,
// a request id, and Anthropic unified-quota headers.
func oaiServerWithUsage() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "req-abc-123")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.42")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", "1772956800")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id": "x", "object": "chat.completion", "model": "m",
			"choices": []interface{}{map[string]interface{}{"index": 0, "finish_reason": "stop",
				"message": map[string]interface{}{"role": "assistant", "content": "ok"}}},
			"usage": map[string]interface{}{
				"prompt_tokens": 100, "completion_tokens": 20,
				"prompt_cache_hit_tokens":   30,
				"completion_tokens_details": map[string]interface{}{"reasoning_tokens": 8},
			},
		})
	}))
}

func TestUsageEvent_NonStreamSuccess(t *testing.T) {
	srv := oaiServerWithUsage()
	defer srv.Close()
	h, db := buildUsageHandler(t, []testProv{{"test", "free", srv.URL}})

	rec := doMessages(h, `{"model":"claude-haiku-4-5","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	evs := requireEvents(t, db, 1)
	e := evs[0]
	if e.Provider != "test" || e.ModelAsked != "claude-haiku-4-5" {
		t.Errorf("provider=%q model_asked=%q", e.Provider, e.ModelAsked)
	}
	if e.Status != 200 || !e.Success || e.Stream || e.Probe {
		t.Errorf("status=%d success=%v stream=%v probe=%v", e.Status, e.Success, e.Stream, e.Probe)
	}
	if e.Attempt != 1 {
		t.Errorf("attempt = %d, want 1", e.Attempt)
	}
	if e.RequestID != "req-abc-123" {
		t.Errorf("request_id = %q, want req-abc-123", e.RequestID)
	}
	if e.KeyIndex != nil {
		t.Errorf("key_index = %v, want nil (no key pool)", *e.KeyIndex)
	}
	// fresh-input semantics: OpenAI prompt_tokens (100) folds in the cached
	// 30, so the event stores In=70 — the same meaning as an Anthropic event.
	// The as-reported prompt_tokens stays reconstructible as in + cache_read.
	wantIntPtr(t, e.In, 70, "in")
	wantIntPtr(t, e.Out, 20, "out")
	wantIntPtr(t, e.CacheRead, 30, "cache_read")
	wantIntPtr(t, e.Reasoning, 8, "reasoning")
	if e.CacheWrite != nil {
		t.Errorf("cache_write = %v, want nil (not reported)", *e.CacheWrite)
	}
	if e.UsagePartial {
		t.Error("usage_partial should be false for a complete response")
	}
	if e.RateLimited || e.Error != "" {
		t.Errorf("rate_limited=%v error=%q", e.RateLimited, e.Error)
	}
	if e.DurationMS < 0 {
		t.Errorf("duration_ms = %d", e.DurationMS)
	}
	// quota transcribed from headers, never computed
	if e.QuotaDimension != "5h" || e.QuotaUtilization == nil || *e.QuotaUtilization != 0.42 {
		t.Errorf("quota dimension=%q util=%v", e.QuotaDimension, e.QuotaUtilization)
	}
	if e.QuotaResetAt == nil || !e.QuotaResetAt.Equal(time.Unix(1772956800, 0).UTC()) {
		t.Errorf("quota_reset_at = %v, want epoch 1772956800", e.QuotaResetAt)
	}
	if !strings.Contains(e.QuotaMeta, "x-request-id") || !strings.Contains(e.QuotaMeta, "anthropic-ratelimit-unified-5h-utilization") {
		t.Errorf("quota_meta should capture raw headers, got %s", e.QuotaMeta)
	}
}

func TestUsageEvent_LiveStreamSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, c := range []string{
			`{"choices":[{"delta":{"content":"Hi"},"finish_reason":null}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":40,"completion_tokens":6,"completion_tokens_details":{"reasoning_tokens":3}}}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", c)
			fl.Flush()
		}
	}))
	defer srv.Close()
	h, db := buildUsageHandler(t, []testProv{{"test", "free", srv.URL}})

	rec := doMessages(h, `{"model":"claude-haiku-4-5","max_tokens":50,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	evs := requireEvents(t, db, 1)
	e := evs[0]
	if !e.Stream {
		t.Error("stream should be true for a live-streamed request")
	}
	wantIntPtr(t, e.In, 40, "in")
	wantIntPtr(t, e.Out, 6, "out")
	wantIntPtr(t, e.Reasoning, 3, "reasoning")
	wantNilIntPtr(t, e.CacheRead, "cache_read")
}

func TestUsageEvent_429Rotation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer good" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id": "x", "object": "chat.completion", "model": "m",
				"choices": []interface{}{map[string]interface{}{"index": 0, "finish_reason": "stop",
					"message": map[string]interface{}{"role": "assistant", "content": "ok"}}},
				"usage": map[string]interface{}{"prompt_tokens": 5, "completion_tokens": 2},
			})
			return
		}
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer srv.Close()

	h, db := buildUsageHandler(t, []testProv{{"p", "free", srv.URL}})
	ap := h.providers["p"]
	ap.keys = []string{"bad", "good"}
	ap.cool = make([]time.Time, 2)
	ap.rr = 0

	rec := doMessages(h, `{"model":"claude-haiku-4-5","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}

	evs := requireEvents(t, db, 2)
	first, second := evs[0], evs[1]
	if first.Status != 429 || !first.RateLimited || first.Success {
		t.Errorf("first event status=%d rate_limited=%v success=%v", first.Status, first.RateLimited, first.Success)
	}
	if first.KeyIndex == nil || *first.KeyIndex != 0 {
		t.Errorf("first key_index = %v, want 0", first.KeyIndex)
	}
	if first.RetryAfter == nil || *first.RetryAfter != 7 {
		t.Errorf("first retry_after = %v, want 7", first.RetryAfter)
	}
	wantNilIntPtr(t, first.In, "429 in") // 429 body carries no usage — nil, never zero
	if second.Status != 200 || second.RateLimited || !second.Success {
		t.Errorf("second event status=%d", second.Status)
	}
	if second.KeyIndex == nil || *second.KeyIndex != 1 {
		t.Errorf("second key_index = %v, want 1", second.KeyIndex)
	}
	wantIntPtr(t, second.In, 5, "second in")
	if second.Attempt != 1 || first.Attempt != 1 {
		t.Errorf("both attempts belong to chain position 1, got %d and %d", first.Attempt, second.Attempt)
	}
}

func TestUsageEvent_FailoverDiscards(t *testing.T) {
	bad := openAIServer(http.StatusInternalServerError, "boom")
	good := oaiServerWithUsage()
	defer bad.Close()
	defer good.Close()
	// free tier first for a simple request → chain: bad, good
	h, db := buildUsageHandler(t, []testProv{{"bad", "free", bad.URL}, {"good", "standard", good.URL}})

	rec := doMessages(h, `{"model":"claude-haiku-4-5","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	// "bad" is retried maxProviderAttempts times (each discard gets an event),
	// then failover records nothing extra — its last 500 was already the
	// third event — and "good" serves: 3 discards + 1 success = 4 events.
	evs := requireEvents(t, db, 4)
	for i := 0; i < 3; i++ {
		e := evs[i]
		if e.Provider != "bad" || e.Status != 500 || e.Success {
			t.Errorf("discard %d: provider=%s status=%d success=%v", i, e.Provider, e.Status, e.Success)
		}
		if e.Attempt != 1 {
			t.Errorf("discard %d: attempt=%d, want 1 (same chain entry)", i, e.Attempt)
		}
		wantNilIntPtr(t, e.In, "discard in")
	}
	last := evs[3]
	if last.Provider != "good" || last.Status != 200 || !last.Success {
		t.Errorf("final: provider=%s status=%d", last.Provider, last.Status)
	}
	if last.Attempt != 2 {
		t.Errorf("final attempt=%d, want 2 (second chain entry)", last.Attempt)
	}
	wantIntPtr(t, last.In, 70, "final in") // fresh portion of prompt_tokens=100 minus cached 30
}

func TestUsageEvent_TransportErrors(t *testing.T) {
	srv := openAIServer(http.StatusOK, "never reached")
	srv.Close() // unreachable

	h, db := buildUsageHandler(t, []testProv{{"p", "free", srv.URL}})

	rec := doMessages(h, `{"model":"claude-haiku-4-5","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502 after all providers fail", rec.Code)
	}

	// the tight same-provider retry fires maxProviderAttempts times; each
	// transport error gets one event with status 0 and the error text
	evs := requireEvents(t, db, maxProviderAttempts)
	for i, e := range evs {
		if e.Status != 0 || e.Success {
			t.Errorf("event %d: status=%d success=%v, want status=0", i, e.Status, e.Success)
		}
		if e.Provider != "p" {
			t.Errorf("event %d: provider=%s", i, e.Provider)
		}
		if !strings.Contains(e.Error, "transport") {
			t.Errorf("event %d: error=%q, want transport prefix", i, e.Error)
		}
		wantNilIntPtr(t, e.In, "transport in")
		if e.RateLimited {
			t.Errorf("event %d: a transport error is never rate-limited", i)
		}
	}
}

// ─── probes ─────────────────────────────────────────────────────────────────

func TestUsageEvent_ProbeSuccessAnd429(t *testing.T) {
	ok := openAIServer(http.StatusOK, "pong")
	defer ok.Close()
	var probe429Hits int32
	ltd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&probe429Hits, 1)
		w.Header().Set("Retry-After", "13")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ltd.Close()

	h, db := buildUsageHandler(t, []testProv{{"ok", "free", ok.URL}, {"ltd", "free", ltd.URL}})
	for name := range h.providers {
		ap := h.providers[name]
		ap.keys = []string{"onlykey"}
		ap.cool = make([]time.Time, 1)
		ap.backoff = make([]time.Duration, 1)
		h.coolKey(ap, name, 0, initialKeyBackoff)
		ap.cool[0] = time.Now().Add(-time.Millisecond) // due for a probe
	}

	h.probeCoolingKeys()

	evs := requireEvents(t, db, 2)
	if probe429Hits != 1 {
		t.Fatalf("429 probe hits = %d, want 1", probe429Hits)
	}
	for _, e := range evs {
		if !e.Probe {
			t.Errorf("probe event not marked probe: %s", dumpEvents([]*storage.UsageEvent{e}))
		}
		if e.Attempt != 0 {
			t.Errorf("probe attempt = %d, want 0 (probes have no chain position)", e.Attempt)
		}
		if e.KeyIndex == nil || *e.KeyIndex != 0 {
			t.Errorf("probe key_index = %v, want 0", e.KeyIndex)
		}
		// only a 200 probe response carries usage; the 429 body has none (nil, not zero)
		if e.Status == 200 {
			wantIntPtr(t, e.In, 10, "probe in")
		} else {
			wantNilIntPtr(t, e.In, "429 probe in")
		}
	}
	if evs[0].Status == 429 && evs[1].Status == 429 {
		t.Errorf("at least one probe should have recovered: %s", dumpEvents(evs))
	}
	for _, e := range evs {
		switch {
		case e.Status == 429:
			if !e.RateLimited || e.RetryAfter == nil || *e.RetryAfter != 13 {
				t.Errorf("429 probe: rate_limited=%v retry_after=%v", e.RateLimited, e.RetryAfter)
			}
		case e.Status == 200:
			if !e.Success {
				t.Errorf("200 probe should be success")
			}
		default:
			t.Errorf("unexpected probe status %d", e.Status)
		}
	}
}

func TestUsageEvent_ProbeTransportError(t *testing.T) {
	srv := openAIServer(http.StatusOK, "x")
	srv.Close() // dead endpoint

	h, db := buildUsageHandler(t, []testProv{{"dead", "free", srv.URL}})
	ap := h.providers["dead"]
	ap.keys = []string{"onlykey"}
	ap.cool = make([]time.Time, 1)
	ap.backoff = make([]time.Duration, 1)
	h.coolKey(ap, "dead", 0, initialKeyBackoff)
	ap.cool[0] = time.Now().Add(-time.Millisecond)

	h.probeCoolingKeys()

	evs := requireEvents(t, db, 1)
	e := evs[0]
	if !e.Probe || e.Status != 0 || !strings.Contains(e.Error, "transport") {
		t.Errorf("probe transport event: probe=%v status=%d error=%q", e.Probe, e.Status, e.Error)
	}
	if ap.hasAvailableKey() {
		t.Error("a failed probe must leave the key cooling")
	}
}

// ─── cascade ─────────────────────────────────────────────────────────────────

func TestUsageEvent_CascadePerAttempt(t *testing.T) {
	var cheapHits, strongHits int32
	cheap := countingOAIServer("", &cheapHits) // empty content → fails verification
	strong := countingOAIServer("solid answer", &strongHits)
	defer cheap.Close()
	defer strong.Close()

	h, db := buildUsageHandler(t, []testProv{{"cheap", "free", cheap.URL}, {"strong", "premium", strong.URL}})
	h.cascade = true

	rec := doMessages(h, `{"model":"claude-sonnet-4-6","max_tokens":100,"messages":[{"role":"user","content":"refactor this"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	if cheapHits != 1 || strongHits != 1 {
		t.Fatalf("cheap=%d strong=%d", cheapHits, strongHits)
	}

	// both cascade attempts consumed real tokens and both get events; the
	// weak one is annotated as discarded, not passed off as an error.
	evs := requireEvents(t, db, 2)
	weak, served := evs[0], evs[1]
	if weak.Provider != "cheap" || weak.Status != 200 || !weak.Success {
		t.Errorf("weak: provider=%s status=%d success=%v", weak.Provider, weak.Status, weak.Success)
	}
	wantIntPtr(t, weak.In, 10, "weak in")
	if !strings.Contains(weak.Error, "cascade") {
		t.Errorf("weak error = %q, want cascade discard annotation", weak.Error)
	}
	if served.Provider != "strong" || served.Status != 200 || !served.Success {
		t.Errorf("served: provider=%s status=%d", served.Provider, served.Status)
	}
	if served.Error != "" {
		t.Errorf("served error = %q, want empty", served.Error)
	}
	wantIntPtr(t, served.In, 10, "served in")
	if weak.Attempt != 1 || served.Attempt != 2 {
		t.Errorf("attempts: weak=%d served=%d, want 1 and 2", weak.Attempt, served.Attempt)
	}
}

// ─── Anthropic-format relays (direct calls; built-in Anthropic has a fixed
// BaseURL so e2e httptest isn't possible for this path) ─────────────────────

// anthropicTestResp fabricates a native Anthropic response with usage detail
// and unified-quota headers.
func anthropicTestResp(body string, status int) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header: http.Header{
			"Anthropic-Ratelimit-Unified-7d-Utilization": []string{"0.9"},
			"Anthropic-Ratelimit-Unified-7d-Reset":       []string{"1772956800000"}, // milliseconds
			"Request-Id":                                 []string{"req-native-42"},
		},
		Body: io.NopCloser(strings.NewReader(body)),
	}
}

const anthropicUsageBody = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-6",` +
	`"content":[{"type":"text","text":"hi"}],` +
	`"usage":{"input_tokens":50,"output_tokens":10,"cache_creation_input_tokens":5,"cache_read_input_tokens":3}}`

// newAnthropicAttempt mirrors what callUpstream does for the winning attempt:
// quota captured once from the response headers, request id extracted.
func newAnthropicAttempt(resp *http.Response) *attemptInfo {
	return &attemptInfo{
		keyIdx:   -1,
		chainPos: 1,
		started:  time.Now(),
		quota:    captureQuota(resp.Header),
		reqID:    extractRequestID(resp.Header),
	}
}

func TestUsageEvent_AnthropicSyncDirect(t *testing.T) {
	h, db := buildUsageHandler(t, nil)
	active := &activeProvider{impl: providers.NewAnthropic(""), apiKey: "sk"}
	req := AnthropicRequest{Model: "claude-sonnet-4-6"}
	resp := anthropicTestResp(anthropicUsageBody, http.StatusOK)
	att := newAnthropicAttempt(resp)

	rec := httptest.NewRecorder()
	h.relayAnthropicSync(rec, active, req, resp, time.Now(), router.ComplexityStandard, att)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	evs := requireEvents(t, db, 1)
	e := evs[0]
	if e.Provider != "anthropic" || e.RequestID != "req-native-42" {
		t.Errorf("provider=%s request_id=%q", e.Provider, e.RequestID)
	}
	wantIntPtr(t, e.In, 50, "in")
	wantIntPtr(t, e.Out, 10, "out")
	wantIntPtr(t, e.CacheRead, 3, "cache_read")
	wantIntPtr(t, e.CacheWrite, 5, "cache_write")
	if e.Reasoning != nil {
		t.Errorf("reasoning = %d, want nil (Anthropic never reports it separately)", *e.Reasoning)
	}
	if e.QuotaDimension != "7d" || e.QuotaUtilization == nil || *e.QuotaUtilization != 0.9 {
		t.Errorf("quota dimension=%q util=%v", e.QuotaDimension, e.QuotaUtilization)
	}
	// millisecond-epoch heuristic: 1772956800000 → same instant as seconds
	if e.QuotaResetAt == nil || !e.QuotaResetAt.Equal(time.Unix(1772956800, 0).UTC()) {
		t.Errorf("quota_reset_at = %v, want ms-epoch 1772956800000", e.QuotaResetAt)
	}
}

func TestUsageEvent_AnthropicBufferedDirect(t *testing.T) {
	h, db := buildUsageHandler(t, nil)
	active := &activeProvider{impl: providers.NewAnthropic(""), apiKey: "sk"}
	req := AnthropicRequest{Model: "claude-sonnet-4-6", Stream: true} // client streams; upstream is buffered

	// 200 with usage
	resp := anthropicTestResp(anthropicUsageBody, http.StatusOK)
	att := newAnthropicAttempt(resp)
	rec := httptest.NewRecorder()
	h.relayAnthropicBuffered(rec, active, req, resp, time.Now(), router.ComplexityStandard, att)
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	evs := requireEvents(t, db, 1)
	wantIntPtr(t, evs[0].In, 50, "in")
	if !evs[0].Stream {
		t.Error("client requested a stream; event.stream should reflect that")
	}

	// error status: no usage in the body → nils, not zeros
	errBody := `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`
	resp2 := anthropicTestResp(errBody, http.StatusServiceUnavailable)
	att2 := newAnthropicAttempt(resp2)
	rec2 := httptest.NewRecorder()
	h.relayAnthropicBuffered(rec2, active, req, resp2, time.Now(), router.ComplexityStandard, att2)
	if rec2.Code != 503 {
		t.Fatalf("status=%d", rec2.Code)
	}
	evs = requireEvents(t, db, 2)
	e := evs[1]
	if e.Status != 503 || e.Success {
		t.Errorf("status=%d success=%v", e.Status, e.Success)
	}
	wantNilIntPtr(t, e.In, "503 in")
	if e.QuotaMeta == "" {
		t.Error("quota headers should be captured even on error responses")
	}
}

func TestUsageEvent_AnthropicStreamDirect(t *testing.T) {
	h, db := buildUsageHandler(t, nil)
	active := &activeProvider{impl: providers.NewAnthropic(""), apiKey: "sk"}
	req := AnthropicRequest{Model: "claude-sonnet-4-6", Stream: true}

	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":50,"cache_creation_input_tokens":5,"cache_read_input_tokens":3}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":10}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}
	att := newAnthropicAttempt(resp)

	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest("POST", "/v1/messages", nil)
	h.relayAnthropicStream(rec, httpReq, active, req, resp, time.Now(), router.ComplexityStandard, att)
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}

	evs := requireEvents(t, db, 1)
	e := evs[0]
	if !e.Stream {
		t.Error("stream should be true")
	}
	wantIntPtr(t, e.In, 50, "in")
	wantIntPtr(t, e.Out, 10, "out")
	wantIntPtr(t, e.CacheRead, 3, "cache_read")
	wantIntPtr(t, e.CacheWrite, 5, "cache_write")
}

// ─── gateway ────────────────────────────────────────────────────────────────

func TestUsageEvent_GatewayPassthrough(t *testing.T) {
	srv := oaiServerWithUsage()
	defer srv.Close()
	h, db := buildUsageHandler(t, []testProv{{"test", "free", srv.URL}})

	rec := chatCompletions(h, `{"model":"gpt-4","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	evs := requireEvents(t, db, 1)
	e := evs[0]
	if e.Provider != "test" || e.Status != 200 || !e.Success {
		t.Errorf("provider=%s status=%d success=%v", e.Provider, e.Status, e.Success)
	}
	if e.Stream {
		t.Error("non-streaming request")
	}
	wantIntPtr(t, e.In, 70, "in") // fresh portion of prompt_tokens=100 minus cached 30
	wantIntPtr(t, e.Reasoning, 8, "reasoning")
	if e.RequestID != "req-abc-123" {
		t.Errorf("request_id=%q", e.RequestID)
	}
}

func TestUsageEvent_GatewayPassthroughStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, c := range []string{
			`{"choices":[{"delta":{"content":"Hi"},"finish_reason":null}]}`,
			`{"choices":[],"usage":{"prompt_tokens":40,"completion_tokens":6}}`,
			`data: [DONE]`,
		} {
			fmt.Fprintf(w, "%s\n", c)
			fl.Flush()
		}
	}))
	defer srv.Close()
	h, db := buildUsageHandler(t, []testProv{{"test", "free", srv.URL}})

	rec := chatCompletions(h, `{"model":"gpt-4","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}

	evs := requireEvents(t, db, 1)
	e := evs[0]
	if !e.Stream {
		t.Error("stream should be true")
	}
	wantIntPtr(t, e.In, 40, "in")
	wantIntPtr(t, e.Out, 6, "out")
}

// ─── safety ─────────────────────────────────────────────────────────────────

func TestUsageEvent_NilAttemptInfoSkipsEvent(t *testing.T) {
	h, db := buildUsageHandler(t, []testProv{{"p", "free", openAIServer(http.StatusOK, "ok").URL}})
	// direct call with a nil att (defensive contract: never panics)
	h.recordUsageEvent(h.providers["p"], AnthropicRequest{Model: "claude-haiku-4-5"}, false, 200, nil, rawUsage{})
	requireEvents(t, db, 0)
}

func TestUsageEvent_NoDBStillServes(t *testing.T) {
	srv := openAIServer(http.StatusOK, "no db configured")
	defer srv.Close()
	h := buildTestHandler(t, []testProv{{"p", "free", srv.URL}}) // no db

	rec := doMessages(h, `{"model":"claude-haiku-4-5","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d — requests must still serve with no DB", rec.Code)
	}
}

func TestUsageEvent_ConcurrentRequests(t *testing.T) {
	srv := openAIServer(http.StatusOK, "ok")
	defer srv.Close()
	h, db := buildUsageHandler(t, []testProv{{"p", "free", srv.URL}})

	const n = 10
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := doMessages(h, `{"model":"claude-haiku-4-5","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`)
			if rec.Code != 200 {
				t.Errorf("status=%d", rec.Code)
			}
		}()
	}
	wg.Wait()

	evs := requireEvents(t, db, n)
	ids := map[int64]bool{}
	for _, e := range evs {
		if ids[e.ID] {
			t.Errorf("duplicate event id %d", e.ID)
		}
		ids[e.ID] = true
	}
}

// ─── U5: aborted-stream handling ───────────────────────────────────────────
//
// A stream that ends without completing — client disconnect or upstream read
// error — must record a partial event (usage_partial=1, observed tokens kept:
// the provider generated and billed them) and mark the requests row's Error
// column with the same abort text. Completed streams must stay unmarked.

// failAfterWriter accepts ok writes and then fails, simulating a client that
// disconnected mid-stream. Relays require a Flusher.
type failAfterWriter struct {
	header http.Header
	ok     int
	writes int
}

func (f *failAfterWriter) Header() http.Header { return f.header }
func (f *failAfterWriter) Write(p []byte) (int, error) {
	f.writes++
	if f.writes > f.ok {
		return 0, errors.New("broken pipe")
	}
	return len(p), nil
}
func (f *failAfterWriter) WriteHeader(int) {}
func (f *failAfterWriter) Flush()          {}

// errAfterBody yields data and then a read error (never EOF) — an upstream
// connection that dies mid-stream.
type errAfterBody struct{ data []byte }

func (b *errAfterBody) Read(p []byte) (int, error) {
	if len(b.data) > 0 {
		n := copy(p, b.data)
		b.data = b.data[n:]
		return n, nil
	}
	return 0, errors.New("connection reset by peer")
}
func (b *errAfterBody) Close() error { return nil }

// yieldingBody produces one SSE chunk per Read until closed — an upstream that
// would keep generating tokens if nobody stopped it. Used to prove the relays
// stop consuming upstream once the client context is cancelled.
type yieldingBody struct {
	chunk []byte
	reads int
	stop  chan struct{}
}

func (b *yieldingBody) Read(p []byte) (int, error) {
	select {
	case <-b.stop:
		return 0, io.EOF
	default:
		b.reads++
		return copy(p, b.chunk), nil
	}
}
func (b *yieldingBody) Close() error { return nil }

// lastRequestRow fetches the most recent requests row.
func lastRequestRow(t *testing.T, db *storage.DB) *storage.Request {
	t.Helper()
	rows, err := db.GetRecentRequests(3)
	if err != nil {
		t.Fatalf("GetRecentRequests: %v", err)
	}
	for _, r := range rows {
		if r.Provider != "cache" {
			return r
		}
	}
	t.Fatalf("no non-cache requests row found")
	return nil
}

const anthropicSSEPartial = "event: message_start\n" +
	`data: {"type":"message_start","message":{"usage":{"input_tokens":50}}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hel"}}` + "\n\n"

func TestAbort_AnthropicStreamClientDisconnect(t *testing.T) {
	h, db := buildUsageHandler(t, nil)
	active := &activeProvider{impl: providers.NewAnthropic(""), apiKey: "sk"}
	req := AnthropicRequest{Model: "claude-sonnet-4-6", Stream: true}

	// first body write fails, but the chunk was already read and captured →
	// the relay must record the partial stream, not complete normally
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(
			anthropicSSEPartial +
				"event: content_block_delta\n" +
				`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"lo more text"}}` + "\n\n")),
	}
	att := newAnthropicAttempt(resp)
	w := &failAfterWriter{header: make(http.Header), ok: 0}
	httpReq := httptest.NewRequest("POST", "/v1/messages", nil)

	h.relayAnthropicStream(w, httpReq, active, req, resp, time.Now(), router.ComplexityStandard, att)

	evs := requireEvents(t, db, 1)
	e := evs[0]
	if !e.UsagePartial || e.Error != "aborted: client disconnected" {
		t.Errorf("event partial=%v error=%q, want partial with client-disconnect mark", e.UsagePartial, e.Error)
	}
	wantIntPtr(t, e.In, 50, "in")  // observed before the abort
	wantNilIntPtr(t, e.Out, "out") // stream never reached message_delta

	row := lastRequestRow(t, db)
	if row.Error != "aborted: client disconnected" {
		t.Errorf("requests row error = %q, want abort mark", row.Error)
	}
}

func TestAbort_AnthropicStreamUpstreamError(t *testing.T) {
	h, db := buildUsageHandler(t, nil)
	active := &activeProvider{impl: providers.NewAnthropic(""), apiKey: "sk"}
	req := AnthropicRequest{Model: "claude-sonnet-4-6", Stream: true}

	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       &errAfterBody{data: []byte(anthropicSSEPartial)},
	}
	att := newAnthropicAttempt(resp)
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest("POST", "/v1/messages", nil)

	h.relayAnthropicStream(rec, httpReq, active, req, resp, time.Now(), router.ComplexityStandard, att)

	evs := requireEvents(t, db, 1)
	e := evs[0]
	if !e.UsagePartial || e.Error != "aborted: upstream stream error" {
		t.Errorf("event partial=%v error=%q, want upstream-error mark", e.UsagePartial, e.Error)
	}
	wantIntPtr(t, e.In, 50, "in")

	if row := lastRequestRow(t, db); row.Error != "aborted: upstream stream error" {
		t.Errorf("requests row error = %q", row.Error)
	}
}

func TestAbort_AnthropicStreamContextCancelled(t *testing.T) {
	h, db := buildUsageHandler(t, nil)
	active := &activeProvider{impl: providers.NewAnthropic(""), apiKey: "sk"}
	req := AnthropicRequest{Model: "claude-sonnet-4-6", Stream: true}

	body := &yieldingBody{
		chunk: []byte(`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"x"}}` + "\n\n"),
		stop:  make(chan struct{}),
	}
	defer func() { go func() { time.Sleep(100 * time.Millisecond); close(body.stop) }() }()

	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       body,
	}
	att := newAnthropicAttempt(resp)
	rec := httptest.NewRecorder()

	// pre-cancelled context: the relay must stop consuming upstream after the
	// first chunk instead of relaying an ever-growing stream
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	httpReq := httptest.NewRequest("POST", "/v1/messages", nil).WithContext(ctx)

	h.relayAnthropicStream(rec, httpReq, active, req, resp, time.Now(), router.ComplexityStandard, att)

	evs := requireEvents(t, db, 1)
	e := evs[0]
	if !e.UsagePartial || e.Error != "aborted: client disconnected" {
		t.Errorf("event partial=%v error=%q", e.UsagePartial, e.Error)
	}
	if body.reads > 3 {
		t.Errorf("upstream consumed %d chunks after client cancellation, want ≤ 3", body.reads)
	}
}

func TestAbort_OpenAIStreamClientDisconnect(t *testing.T) {
	h, db := buildUsageHandler(t, nil)
	active := &activeProvider{impl: providers.NewGroq("k")}
	req := AnthropicRequest{Model: "claude-haiku-4-5", Stream: true}

	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Hello "},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":"world"},"finish_reason":null}]}`,
	}, "\n") + "\n"
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(sse)), Header: http.Header{}}
	att := newAnthropicAttempt(resp)
	w := &failAfterWriter{header: make(http.Header), ok: 2} // message_start + first delta OK, then fail
	httpReq := httptest.NewRequest("POST", "/v1/messages", nil)

	h.relayOpenAIStream(w, httpReq, active, req, resp, time.Now(), router.ComplexitySimple, att)

	evs := requireEvents(t, db, 1)
	e := evs[0]
	if !e.UsagePartial || e.Error != "aborted: client disconnected" {
		t.Errorf("event partial=%v error=%q", e.UsagePartial, e.Error)
	}
	// usage chunk never arrived → counts unknown, not zero
	wantNilIntPtr(t, e.In, "in")
	if row := lastRequestRow(t, db); row.Error != "aborted: client disconnected" {
		t.Errorf("requests row error = %q", row.Error)
	}
}

func TestAbort_OpenAIStreamUpstreamError(t *testing.T) {
	h, db := buildUsageHandler(t, nil)
	active := &activeProvider{impl: providers.NewGroq("k")}
	req := AnthropicRequest{Model: "claude-haiku-4-5", Stream: true}

	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Hi"},"finish_reason":null}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":40,"completion_tokens":6}}`,
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: 200, Header: http.Header{},
		Body: &errAfterBody{data: []byte(sse)},
	}
	att := newAnthropicAttempt(resp)
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest("POST", "/v1/messages", nil)

	h.relayOpenAIStream(rec, httpReq, active, req, resp, time.Now(), router.ComplexitySimple, att)

	evs := requireEvents(t, db, 1)
	e := evs[0]
	if !e.UsagePartial || e.Error != "aborted: upstream stream error" {
		t.Errorf("event partial=%v error=%q", e.UsagePartial, e.Error)
	}
	// usage chunk arrived before the connection died → keep the real counts
	wantIntPtr(t, e.In, 40, "in")
	wantIntPtr(t, e.Out, 6, "out")
	if row := lastRequestRow(t, db); row.Error != "aborted: upstream stream error" {
		t.Errorf("requests row error = %q", row.Error)
	}
}

func TestAbort_OpenAIStreamContextCancelled(t *testing.T) {
	h, db := buildUsageHandler(t, nil)
	active := &activeProvider{impl: providers.NewGroq("k")}
	req := AnthropicRequest{Model: "claude-haiku-4-5", Stream: true}

	body := &yieldingBody{
		chunk: []byte(`data: {"choices":[{"delta":{"content":"x"},"finish_reason":null}]}` + "\n\n"),
		stop:  make(chan struct{}),
	}
	defer func() { go func() { time.Sleep(100 * time.Millisecond); close(body.stop) }() }()

	resp := &http.Response{StatusCode: 200, Body: body, Header: http.Header{}}
	att := newAnthropicAttempt(resp)
	rec := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	httpReq := httptest.NewRequest("POST", "/v1/messages", nil).WithContext(ctx)

	h.relayOpenAIStream(rec, httpReq, active, req, resp, time.Now(), router.ComplexitySimple, att)

	evs := requireEvents(t, db, 1)
	if !evs[0].UsagePartial || evs[0].Error != "aborted: client disconnected" {
		t.Errorf("event partial=%v error=%q", evs[0].UsagePartial, evs[0].Error)
	}
	if body.reads > 3 {
		t.Errorf("upstream consumed %d chunks after client cancellation, want ≤ 3", body.reads)
	}
}

func TestAbort_PassthroughStreamClientDisconnect(t *testing.T) {
	h, db := buildUsageHandler(t, nil)
	active := &activeProvider{impl: providers.NewGroq("k")}
	areq := AnthropicRequest{Model: "gpt-4"}

	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Hello "},"finish_reason":null}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":40,"completion_tokens":6}}`,
		"",
	}, "\n")
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(sse)), Header: http.Header{}}
	att := newAnthropicAttempt(resp)
	w := &failAfterWriter{header: make(http.Header), ok: 0} // first write fails
	httpReq := httptest.NewRequest("POST", "/v1/chat/completions", nil)

	h.relayOpenAIPassthroughStream(w, httpReq, active, areq, resp, time.Now(), router.ComplexitySimple, att)

	evs := requireEvents(t, db, 1)
	e := evs[0]
	if !e.UsagePartial || e.Error != "aborted: client disconnected" {
		t.Errorf("event partial=%v error=%q", e.UsagePartial, e.Error)
	}
	if row := lastRequestRow(t, db); row.Error != "aborted: client disconnected" {
		t.Errorf("requests row error = %q", row.Error)
	}
}

func TestAbort_PassthroughStreamUpstreamError(t *testing.T) {
	h, db := buildUsageHandler(t, nil)
	active := &activeProvider{impl: providers.NewGroq("k")}
	areq := AnthropicRequest{Model: "gpt-4"}

	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Hi"},"finish_reason":null}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":40,"completion_tokens":6}}`,
		"",
	}, "\n")
	resp := &http.Response{StatusCode: 200, Body: &errAfterBody{data: []byte(sse)}, Header: http.Header{}}
	att := newAnthropicAttempt(resp)
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest("POST", "/v1/chat/completions", nil)

	h.relayOpenAIPassthroughStream(rec, httpReq, active, areq, resp, time.Now(), router.ComplexitySimple, att)

	evs := requireEvents(t, db, 1)
	e := evs[0]
	if !e.UsagePartial || e.Error != "aborted: upstream stream error" {
		t.Errorf("event partial=%v error=%q", e.UsagePartial, e.Error)
	}
	// usage line was captured before the break — the regex scrape finds it
	wantIntPtr(t, e.In, 40, "in")
}

func TestAbort_CompletedStreamsNotMarked(t *testing.T) {
	h, db := buildUsageHandler(t, nil)
	active := &activeProvider{impl: providers.NewAnthropic(""), apiKey: "sk"}
	req := AnthropicRequest{Model: "claude-sonnet-4-6", Stream: true}

	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":12}}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	resp := &http.Response{
		StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(sse)),
	}
	att := newAnthropicAttempt(resp)

	rec := httptest.NewRecorder()
	h.relayAnthropicStream(rec, httptest.NewRequest("POST", "/v1/messages", nil), active, req, resp, time.Now(), router.ComplexityStandard, att)

	evs := requireEvents(t, db, 1)
	if evs[0].UsagePartial || evs[0].Error != "" {
		t.Errorf("completed stream must not be marked partial: partial=%v error=%q", evs[0].UsagePartial, evs[0].Error)
	}
	if row := lastRequestRow(t, db); row.Error != "" {
		t.Errorf("completed stream requests row error = %q, want empty", row.Error)
	}
}

// A gateway transport error must still attribute the model: inModel is in
// scope at the recording site, so the discard event names what was asked,
// like every other discard site.
func TestUsageEvent_GatewayTransportError(t *testing.T) {
	srv := openAIServer(http.StatusOK, "never reached")
	srv.Close() // unreachable

	h, db := buildUsageHandler(t, []testProv{{"test", "free", srv.URL}})
	rec := chatCompletions(h, `{"model":"gpt-4","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502", rec.Code)
	}
	// the passthrough has no same-provider retry loop: one transport error
	// → one event, then the chain ends and the client sees 502
	evs := requireEvents(t, db, 1)
	for i, e := range evs {
		if e.Status != 0 || e.Success {
			t.Errorf("event %d: status=%d success=%v", i, e.Status, e.Success)
		}
		if e.ModelAsked != "gpt-4" {
			t.Errorf("event %d: model_asked=%q, want gpt-4 (transport discards keep model attribution)", i, e.ModelAsked)
		}
		if !strings.Contains(e.Error, "transport") {
			t.Errorf("event %d: error=%q", i, e.Error)
		}
	}
}

// The abort path's cached-token split: the event keeps the as-reported prompt
// (raw.In=100) plus the cached portion, recordUsageEvent re-derives fresh
// input, and the requests row's cost view must match — the raw/cost pair
// can't drift because both come from oaiStreamUsageView.
func TestAbort_OpenAIStreamCachedUsage(t *testing.T) {
	h, db := buildUsageHandler(t, nil)
	active := &activeProvider{impl: providers.NewGroq("k")}
	req := AnthropicRequest{Model: "claude-haiku-4-5", Stream: true}

	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Hi"},"finish_reason":null}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":6,"prompt_tokens_details":{"cached_tokens":40}}}`,
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: 200, Header: http.Header{},
		Body: &errAfterBody{data: []byte(sse)},
	}
	att := newAnthropicAttempt(resp)
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest("POST", "/v1/messages", nil)

	h.relayOpenAIStream(rec, httpReq, active, req, resp, time.Now(), router.ComplexitySimple, att)

	evs := requireEvents(t, db, 1)
	e := evs[0]
	if !e.UsagePartial || e.Error != "aborted: upstream stream error" {
		t.Errorf("event partial=%v error=%q", e.UsagePartial, e.Error)
	}
	wantIntPtr(t, e.CacheRead, 40, "cache_read")
	wantIntPtr(t, e.Out, 6, "out")
	// fresh input: 100 reported − 40 cached, after the OpenAI normalization
	wantIntPtr(t, e.In, 60, "in")
	// cost view agrees with the event (fresh input, not the raw prompt)
	if row := lastRequestRow(t, db); row.InputTokens != 60 || row.CacheReadTokens != 40 {
		t.Errorf("requests row in=%d cache_read=%d, want 60/40", row.InputTokens, row.CacheReadTokens)
	}
}

// A malformed upstream reporting cached > prompt must not produce negative
// fresh input in either the event or the cost view (the clamp in
// oaiStreamUsageView, shared by abort and completed-stream paths).
func TestAbort_OpenAIStreamCachedExceedsPrompt(t *testing.T) {
	h, db := buildUsageHandler(t, nil)
	active := &activeProvider{impl: providers.NewGroq("k")}
	req := AnthropicRequest{Model: "claude-haiku-4-5", Stream: true}

	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Hi"},"finish_reason":null}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":30,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":150}}}`,
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: 200, Header: http.Header{},
		Body: &errAfterBody{data: []byte(sse)},
	}
	att := newAnthropicAttempt(resp)
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest("POST", "/v1/messages", nil)

	h.relayOpenAIStream(rec, httpReq, active, req, resp, time.Now(), router.ComplexitySimple, att)

	e := requireEvents(t, db, 1)[0]
	wantIntPtr(t, e.In, 0, "in (clamped, never negative)")
	wantIntPtr(t, e.CacheRead, 30, "cache_read (clamped to prompt)")
	if row := lastRequestRow(t, db); row.InputTokens != 0 {
		t.Errorf("requests row in=%d, want 0 (clamped)", row.InputTokens)
	}
}
