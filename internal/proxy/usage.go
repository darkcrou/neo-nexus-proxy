package proxy

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// tokenUsage is a normalized token breakdown across providers.
//
//   - In         — full-price input tokens
//   - Out        — output tokens
//   - CacheRead  — tokens served from the provider's prompt cache (billed at the
//     cache-read discount, typically ~0.1× input)
//   - CacheWrite — tokens written into the provider's prompt cache (billed at the
//     cache-write premium, ~1.25× input for Anthropic)
//
// For Anthropic, `input_tokens` already excludes cached tokens, so In maps
// straight across. For OpenAI-compatible providers the cached tokens are part of
// `prompt_tokens`, so we subtract them out — In is always the full-price portion.
type tokenUsage struct {
	In, Out, CacheRead, CacheWrite int
}

// billable is the total token count touched (for logging/aggregation).
func (u tokenUsage) billable() int { return u.In + u.Out + u.CacheRead + u.CacheWrite }

// ─── Raw usage (presence-preserving) ────────────────────────────────────────
//
// rawUsage preserves exactly what the provider reported: nil means "not
// reported", a reported zero stays zero. It feeds the immutable usage_events
// store; the normalized tokenUsage views below are derived from it for cost
// calculation so the two can never drift apart.

type rawUsage struct {
	In         *int // as-reported input (OpenAI: includes cached; Anthropic: excludes)
	Out        *int
	CacheRead  *int
	CacheWrite *int
	Reasoning  *int // subset of Out — never double-counted
}

func derefInt(p *int) int {
	if p != nil {
		return *p
	}
	return 0
}

// anthropicTokens converts an Anthropic-format report to the normalized cost
// view. Anthropic reports cache fields separately from input, so values map
// straight across and absent fields count as zero.
func (r rawUsage) anthropicTokens() tokenUsage {
	return tokenUsage{
		In:         derefInt(r.In),
		Out:        derefInt(r.Out),
		CacheRead:  derefInt(r.CacheRead),
		CacheWrite: derefInt(r.CacheWrite),
	}
}

// openAITokens converts an OpenAI-compatible report to the normalized cost
// view. Cached tokens are part of prompt_tokens, so In is the uncached
// remainder; a cached count exceeding the prompt is clamped.
func (r rawUsage) openAITokens() tokenUsage {
	in, cached := derefInt(r.In), derefInt(r.CacheRead)
	if cached > in {
		cached = in
	}
	return tokenUsage{In: in - cached, Out: derefInt(r.Out), CacheRead: cached}
}

var (
	reCacheRead     = regexp.MustCompile(`"cache_read_input_tokens":\s*(\d+)`)
	reCacheCreation = regexp.MustCompile(`"cache_creation_input_tokens":\s*(\d+)`)
	reOAICacheHit   = regexp.MustCompile(`"prompt_cache_hit_tokens":\s*(\d+)`) // DeepSeek
	reOAICached     = regexp.MustCompile(`"cached_tokens":\s*(\d+)`)           // OpenAI prompt_tokens_details
	reReasoning     = regexp.MustCompile(`"reasoning_tokens":\s*(\d+)`)        // GLM/OpenAI completion_tokens_details
)

func atoiBytes(b []byte) int { n, _ := strconv.Atoi(string(b)); return n }

// anthropicRawUsage parses a non-streaming Anthropic response body, preserving
// which fields were actually reported. Anthropic does not report reasoning
// tokens separately, so Reasoning stays nil.
func anthropicRawUsage(body []byte) rawUsage {
	var r struct {
		Usage struct {
			InputTokens              *int `json:"input_tokens"`
			OutputTokens             *int `json:"output_tokens"`
			CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(body, &r)
	return rawUsage{
		In:         r.Usage.InputTokens,
		Out:        r.Usage.OutputTokens,
		CacheRead:  r.Usage.CacheReadInputTokens,
		CacheWrite: r.Usage.CacheCreationInputTokens,
	}
}

// streamRawUsage scrapes usage from a captured Anthropic SSE stream, preserving
// presence: input/cache counts appear once (message_start); output_tokens
// appears multiple times (message_delta), so the final value wins. A truncated
// capture simply reports fewer fields.
func streamRawUsage(data []byte) rawUsage {
	u := rawUsage{}
	if m := reInputTokens.FindSubmatch(data); m != nil {
		v := atoiBytes(m[1])
		u.In = &v
	}
	if all := reOutputTokens.FindAllSubmatch(data, -1); len(all) > 0 {
		v := atoiBytes(all[len(all)-1][1])
		u.Out = &v
	}
	if m := reCacheRead.FindSubmatch(data); m != nil {
		v := atoiBytes(m[1])
		u.CacheRead = &v
	}
	if m := reCacheCreation.FindSubmatch(data); m != nil {
		v := atoiBytes(m[1])
		u.CacheWrite = &v
	}
	return u
}

// openAIRawUsage parses an OpenAI-compatible report (full body or captured
// stream), preserving presence. DeepSeek reports prompt_cache_hit_tokens; OpenAI
// reports cached_tokens inside prompt_tokens_details; GLM reasoning models report
// reasoning_tokens inside completion_tokens_details.
func openAIRawUsage(data []byte) rawUsage {
	u := rawUsage{}
	if m := reOAIPrompt.FindSubmatch(data); m != nil {
		v := atoiBytes(m[1])
		u.In = &v
	}
	if all := reOAICompletion.FindAllSubmatch(data, -1); len(all) > 0 {
		v := atoiBytes(all[len(all)-1][1])
		u.Out = &v
	}
	if m := reOAICacheHit.FindSubmatch(data); m != nil {
		v := atoiBytes(m[1])
		u.CacheRead = &v
	} else if m := reOAICached.FindSubmatch(data); m != nil {
		v := atoiBytes(m[1])
		u.CacheRead = &v
	}
	if m := reReasoning.FindSubmatch(data); m != nil {
		v := atoiBytes(m[1])
		u.Reasoning = &v
	}
	return u
}

// ─── Normalized cost-path views (unchanged arithmetic) ──────────────────────

// anthropicUsageFull parses a non-streaming Anthropic response body.
func anthropicUsageFull(body []byte) tokenUsage {
	return anthropicRawUsage(body).anthropicTokens()
}

// streamUsageFull scrapes token counts from a captured Anthropic SSE stream.
func streamUsageFull(data []byte) tokenUsage {
	return streamRawUsage(data).anthropicTokens()
}

// openAIUsageFull parses an OpenAI/DeepSeek response (full body or captured
// stream). Cached tokens are part of prompt_tokens, so they are moved into
// CacheRead and In is left as the uncached remainder.
func openAIUsageFull(data []byte) tokenUsage {
	return openAIRawUsage(data).openAITokens()
}

// ─── Quota / rate-limit header capture ──────────────────────────────────────
//
// attemptQuota transcribes provider-reported rate-limit and quota information
// from a response's headers. Values are copied exactly as reported — NEXUS
// never computes or estimates utilization locally. Anything unrecognized is
// still preserved verbatim in Meta for later analysis.

type attemptQuota struct {
	Dimension    string     // normalized quota dimension ("5h", "7d"); "" = none labeled
	Utilization  *float64   // 0..1 fraction, exactly as reported
	ResetAt      *time.Time // provider-reported quota reset epoch
	RetryAfter   *float64   // seconds, from a 429's Retry-After
	RetryResetAt *time.Time // absolute retry time (Retry-After date or reset-timestamp header)
	Meta         string     // JSON map of raw captured rate-limit/quota headers
}

// quotaHeaderPrefixes names header families whose raw values are captured into
// Meta verbatim. Format-based, not provider-name-based: any provider emitting
// these headers gets captured.
var quotaHeaderPrefixes = []string{
	"anthropic-ratelimit-", // Anthropic rate-limit/quota family (unified + api-key)
	"x-ratelimit-",         // generic rate-limit family (x-ratelimit-limit-requests, ...)
	"x-quota-",             // generic quota family
	"x-ollama-",            // Ollama-specific family
	"ratelimit-",           // some providers use non-x-prefixed names
	"x-remaining-",         // some gateways report remaining budget
	"retry-after",          // retry-after + retry-after-reset-timestamp
}

// quotaHeaderExact are single headers captured into Meta (correlation ids).
var quotaHeaderExact = map[string]bool{
	"request-id":           true,
	"x-request-id":         true,
	"anthropic-request-id": true,
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// captureQuota reads quota/rate-limit information off a response's headers.
// Normalized fields are transcribed only from explicit provider-reported
// utilization headers (Anthropic's unified-quota family); everything else is
// preserved raw in Meta.
func captureQuota(h http.Header) attemptQuota {
	q := attemptQuota{}

	captured := map[string]string{}
	for name, vals := range h {
		ln := strings.ToLower(name)
		if quotaHeaderExact[ln] || hasAnyPrefix(ln, quotaHeaderPrefixes) {
			captured[ln] = strings.Join(vals, ", ")
		}
	}
	if len(captured) > 0 {
		if b, err := json.Marshal(captured); err == nil {
			q.Meta = string(b)
		}
	}

	// Anthropic unified-quota transcription: prefer the explicit per-dimension
	// headers; fall back to the representative claim's reset epoch alone.
	for _, key := range []string{"5h", "7d"} {
		v := h.Get("Anthropic-Ratelimit-Unified-" + key + "-Utilization")
		if v == "" {
			continue
		}
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			q.Dimension = key
			q.Utilization = &f
			if r := h.Get("Anthropic-Ratelimit-Unified-" + key + "-Reset"); r != "" {
				q.ResetAt = parseEpochHeader(r)
			}
			break
		}
	}
	if q.Dimension == "" {
		if claim := h.Get("Anthropic-Ratelimit-Unified-Representative-Claim"); claim != "" {
			q.Dimension = normalizeQuotaClaim(claim)
			if r := h.Get("Anthropic-Ratelimit-Unified-Reset"); r != "" {
				q.ResetAt = parseEpochHeader(r)
			}
		}
	}

	// Retry-After: numeric values are seconds; date values are absolute times.
	if v := h.Get("Retry-After"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			q.RetryAfter = &f
		} else if t, err := http.ParseTime(v); err == nil {
			u := t.UTC()
			q.RetryResetAt = &u
		}
	}
	if v := h.Get("Retry-After-Reset-Timestamp"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			u := t.UTC()
			q.RetryResetAt = &u
		}
	}
	return q
}

// normalizeQuotaClaim maps Anthropic's representative-claim vocabulary onto
// window dimension labels ("five_hour" → "5h", "seven_day*" → "7d").
func normalizeQuotaClaim(claim string) string {
	switch {
	case strings.Contains(claim, "five_hour"):
		return "5h"
	case strings.Contains(claim, "seven_day"):
		return "7d"
	}
	return ""
}

// parseEpochHeader parses a unix-epoch header value. Values above 1e12 are
// treated as milliseconds (seconds epochs stay below 1e12 until year 2286).
func parseEpochHeader(v string) *time.Time {
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return nil
	}
	var t time.Time
	if f > 1e12 {
		t = time.UnixMilli(int64(f)).UTC()
	} else {
		t = time.Unix(int64(f), 0).UTC()
	}
	return &t
}