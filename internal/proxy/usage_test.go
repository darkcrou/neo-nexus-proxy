package proxy

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAnthropicUsageFull(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":900,"cache_creation_input_tokens":200}}`)
	u := anthropicUsageFull(body)
	if u.In != 100 || u.Out != 50 || u.CacheRead != 900 || u.CacheWrite != 200 {
		t.Fatalf("got %+v", u)
	}
}

func TestOpenAIUsageFull_DeepSeek(t *testing.T) {
	// DeepSeek reports prompt_cache_hit_tokens; it is part of prompt_tokens.
	body := []byte(`{"usage":{"prompt_tokens":1000,"completion_tokens":50,"prompt_cache_hit_tokens":800}}`)
	u := openAIUsageFull(body)
	if u.In != 200 || u.Out != 50 || u.CacheRead != 800 {
		t.Fatalf("got %+v", u)
	}
}

func TestOpenAIUsageFull_OpenAI(t *testing.T) {
	// OpenAI reports cached_tokens inside prompt_tokens_details.
	body := []byte(`{"usage":{"prompt_tokens":1000,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":640}}}`)
	u := openAIUsageFull(body)
	if u.In != 360 || u.Out != 50 || u.CacheRead != 640 {
		t.Fatalf("got %+v", u)
	}
}

func TestOpenAIUsageFull_NoCache(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":300,"completion_tokens":40}}`)
	u := openAIUsageFull(body)
	if u.In != 300 || u.Out != 40 || u.CacheRead != 0 {
		t.Fatalf("got %+v", u)
	}
}

func TestStreamUsageFull(t *testing.T) {
	// message_start carries input + cache; message_delta carries output (twice).
	stream := []byte(`event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":120,"cache_read_input_tokens":4000,"cache_creation_input_tokens":300,"output_tokens":0}}}

event: message_delta
data: {"type":"message_delta","usage":{"output_tokens":10}}

event: message_delta
data: {"type":"message_delta","usage":{"output_tokens":85}}
`)
	u := streamUsageFull(stream)
	if u.In != 120 || u.Out != 85 || u.CacheRead != 4000 || u.CacheWrite != 300 {
		t.Fatalf("got %+v", u)
	}
}

// ─── U3: presence-aware raw usage ───────────────────────────────────────────

func TestAnthropicRawUsagePresence(t *testing.T) {
	// Full report: every field present.
	body := []byte(`{"usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":900,"cache_creation_input_tokens":200}}`)
	r := anthropicRawUsage(body)
	if r.In == nil || *r.In != 100 || r.Out == nil || *r.Out != 50 ||
		r.CacheRead == nil || *r.CacheRead != 900 || r.CacheWrite == nil || *r.CacheWrite != 200 {
		t.Fatalf("full report wrong: %+v", r)
	}
	if r.Reasoning != nil {
		t.Errorf("Anthropic never reports reasoning tokens: %+v", r.Reasoning)
	}

	// Partial report: absent fields must stay nil, never zero.
	partial := []byte(`{"usage":{"input_tokens":10}}`)
	r = anthropicRawUsage(partial)
	if r.In == nil || *r.In != 10 {
		t.Fatalf("input lost: %+v", r)
	}
	if r.Out != nil || r.CacheRead != nil || r.CacheWrite != nil {
		t.Fatalf("absent fields must stay nil: %+v", r)
	}

	// Missing usage entirely.
	empty := []byte(`{"type":"error"}`)
	r = anthropicRawUsage(empty)
	if r.In != nil || r.Out != nil {
		t.Fatalf("missing usage must be all nil: %+v", r)
	}
}

func TestStreamRawUsage(t *testing.T) {
	stream := []byte(`event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":120,"cache_read_input_tokens":4000,"cache_creation_input_tokens":300,"output_tokens":0}}}

event: message_delta
data: {"type":"message_delta","usage":{"output_tokens":10}}

event: message_delta
data: {"type":"message_delta","usage":{"output_tokens":85}}
`)
	r := streamRawUsage(stream)
	if r.In == nil || *r.In != 120 || r.Out == nil || *r.Out != 85 ||
		r.CacheRead == nil || *r.CacheRead != 4000 || r.CacheWrite == nil || *r.CacheWrite != 300 {
		t.Fatalf("stream raw usage wrong: %+v", r)
	}
	// A truncated capture (no message_delta yet) reports what it has.
	trunc := []byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":50,"output_tokens":0}}}`)
	r = streamRawUsage(trunc)
	if r.In == nil || *r.In != 50 || r.Out == nil || *r.Out != 0 {
		t.Fatalf("truncated stream wrong: %+v", r)
	}
}

func TestOpenAIRawUsage_Reasoning(t *testing.T) {
	// GLM reasoning models report completion_tokens_details.reasoning_tokens.
	body := []byte(`{"usage":{"prompt_tokens":1000,"completion_tokens":500,"completion_tokens_details":{"reasoning_tokens":200},"prompt_tokens_details":{"cached_tokens":400}}}`)
	r := openAIRawUsage(body)
	if r.In == nil || *r.In != 1000 || r.Out == nil || *r.Out != 500 {
		t.Fatalf("base tokens wrong: %+v", r)
	}
	if r.Reasoning == nil || *r.Reasoning != 200 {
		t.Errorf("reasoning tokens lost: %+v", r.Reasoning)
	}
	if r.CacheRead == nil || *r.CacheRead != 400 {
		t.Errorf("cached tokens lost: %+v", r.CacheRead)
	}
}

func TestOpenAIRawUsage_AbsentFieldsNil(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":300,"completion_tokens":40}}`)
	r := openAIRawUsage(body)
	if r.In == nil || *r.In != 300 || r.Out == nil || *r.Out != 40 {
		t.Fatalf("base tokens wrong: %+v", r)
	}
	if r.CacheRead != nil || r.Reasoning != nil {
		t.Fatalf("absent fields must stay nil: %+v", r)
	}
	// Normalized cost view stays identical to the legacy behavior.
	u := r.openAITokens()
	if u.In != 300 || u.Out != 40 || u.CacheRead != 0 {
		t.Errorf("normalization regression: %+v", u)
	}
}

func TestRawUsageNormalizationMatchesLegacy(t *testing.T) {
	// DeepSeek: cached > reported? clamp path stays intact.
	body := []byte(`{"usage":{"prompt_tokens":500,"completion_tokens":50,"prompt_cache_hit_tokens":800}}`)
	r := openAIRawUsage(body)
	if r.CacheRead == nil || *r.CacheRead != 800 {
		t.Fatalf("raw cache lost: %+v", r)
	}
	u := r.openAITokens()
	if u.CacheRead != 500 || u.In != 0 {
		t.Errorf("clamp regression: %+v", u)
	}
}

// ─── U3: quota header capture ───────────────────────────────────────────────

func TestCaptureQuota_AnthropicUnified(t *testing.T) {
	reset := time.Now().Add(2 * time.Hour).UTC()
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-Status", "allowed_warning")
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.42")
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", strconv.FormatInt(reset.Unix(), 10))
	h.Set("Anthropic-Ratelimit-Unified-Representative-Claim", "five_hour")
	h.Set("Request-Id", "req_123")

	q := captureQuota(h)
	if q.Dimension != "5h" {
		t.Errorf("dimension: want 5h, got %q", q.Dimension)
	}
	if q.Utilization == nil || *q.Utilization != 0.42 {
		t.Errorf("utilization: want 0.42, got %v", q.Utilization)
	}
	if q.ResetAt == nil || q.ResetAt.Unix() != reset.Unix() {
		t.Errorf("reset epoch: want %v, got %v", reset.Unix(), q.ResetAt)
	}
	// Raw headers must survive verbatim in Meta.
	for _, want := range []string{`"anthropic-ratelimit-unified-status":"allowed_warning"`,
		`"anthropic-ratelimit-unified-5h-utilization":"0.42"`, `"request-id":"req_123"`} {
		if !strings.Contains(q.Meta, want) {
			t.Errorf("Meta JSON missing %s: %s", want, q.Meta)
		}
	}
}

func TestCaptureQuota_RepresentativeClaimOnly(t *testing.T) {
	// No per-dimension utilization — just the claim + its reset epoch.
	reset := time.Now().Add(90 * time.Minute).UTC()
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-Representative-Claim", "seven_day")
	h.Set("Anthropic-Ratelimit-Unified-Reset", strconv.FormatInt(reset.Unix(), 10))

	q := captureQuota(h)
	if q.Dimension != "7d" {
		t.Errorf("claim normalization: want 7d, got %q", q.Dimension)
	}
	if q.Utilization != nil {
		t.Errorf("utilization must stay nil when not reported: %v", *q.Utilization)
	}
	if q.ResetAt == nil || q.ResetAt.Unix() != reset.Unix() {
		t.Errorf("reset from claim: %v", q.ResetAt)
	}
}

func TestCaptureQuota_RetryAfter(t *testing.T) {
	// Plain seconds.
	h := http.Header{}
	h.Set("Retry-After", "30")
	q := captureQuota(h)
	if q.RetryAfter == nil || *q.RetryAfter != 30 {
		t.Errorf("retry-after seconds lost: %+v", q)
	}
	// Absolute timestamp form.
	reset := time.Now().Add(time.Minute).UTC()
	h = http.Header{}
	h.Set("Retry-After-Reset-Timestamp", reset.Format(time.RFC3339))
	q = captureQuota(h)
	if q.RetryResetAt == nil || !q.RetryResetAt.Equal(reset.Truncate(time.Second)) {
		t.Errorf("retry reset timestamp lost: %v", q.RetryResetAt)
	}
	if q.RetryAfter != nil {
		t.Errorf("no seconds form present: %+v", *q.RetryAfter)
	}
	// HTTP-date form.
	h = http.Header{}
	h.Set("Retry-After", time.Now().Add(2*time.Minute).UTC().Format(http.TimeFormat))
	q = captureQuota(h)
	if q.RetryResetAt == nil {
		t.Errorf("HTTP-date retry-after lost: %+v", q)
	}
	if q.RetryAfter != nil {
		t.Errorf("HTTP-date must not parse as seconds: %+v", *q.RetryAfter)
	}
}

func TestCaptureQuota_GenericHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("X-Ratelimit-Remaining-Requests", "41")
	h.Set("X-Ratelimit-Reset-Requests", "1m2s")
	h.Set("X-Quota-Remaining", "7")
	h.Set("Content-Type", "application/json") // never captured

	q := captureQuota(h)
	if q.Dimension != "" || q.Utilization != nil || q.ResetAt != nil {
		t.Errorf("generic headers must not be transcribed into normalized fields: %+v", q)
	}
	for _, want := range []string{`"x-ratelimit-remaining-requests":"41"`, `"x-quota-remaining":"7"`} {
		if !strings.Contains(q.Meta, want) {
			t.Errorf("Meta JSON missing %s: %s", want, q.Meta)
		}
	}
	if strings.Contains(q.Meta, "content-type") {
		t.Errorf("unrelated header leaked into Meta: %s", q.Meta)
	}
}

func TestCaptureQuota_Empty(t *testing.T) {
	q := captureQuota(http.Header{})
	if q.Dimension != "" || q.Utilization != nil || q.ResetAt != nil ||
		q.RetryAfter != nil || q.RetryResetAt != nil || q.Meta != "" {
		t.Errorf("no headers must yield zero-value quota: %+v", q)
	}
}
