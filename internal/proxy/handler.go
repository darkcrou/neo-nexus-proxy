// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NEXUS contributors

package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/lynuxis2026-pixel/nexus-proxy/internal/config"
	"github.com/lynuxis2026-pixel/nexus-proxy/internal/providers"
	"github.com/lynuxis2026-pixel/nexus-proxy/internal/router"
	"github.com/lynuxis2026-pixel/nexus-proxy/internal/storage"
)

// EventPublisher lets the handler push live events to the dashboard over SSE.
// (*dashboard.SSEBroker satisfies this — passed in from main to avoid coupling.)
type EventPublisher interface {
	Publish(eventType string, data interface{})
}

// activeProvider bundles a provider implementation with its API key pool.
// NEXUS pins to one "sticky" key per provider and keeps using it until it
// returns a 429, at which point it cools that key and moves on to the next
// non-cooling one — so a pool of free-tier keys behaves like one larger free
// quota, without needlessly spreading load across keys that are still fine.
type activeProvider struct {
	impl   providers.Provider
	apiKey string // primary key (keys[0]) — used for health checks and single-key paths

	mu      sync.Mutex
	keys    []string
	rr      uint32          // sticky "current key" index (not a round-robin cursor)
	cool    []time.Time     // per-key "cooling until" timestamps
	backoff []time.Duration // per-key current backoff duration (0 = never penalized / recovered)
}

// initialKeyBackoff is the cooldown a key gets on its first 429. keyProbeLoop
// doubles it (capped at maxKeyBackoff) each time a recovery probe still sees
// a 429, so a briefly-rate-limited free key is retried quickly while a
// long-exhausted paid key backs off to a much slower probe cadence instead of
// being hammered for hours.
const (
	initialKeyBackoff = 10 * time.Second
	maxKeyBackoff     = 5 * time.Minute
)

// pickKey returns the sticky current key: the same key on repeated calls
// while it isn't cooling. Only when the current key is cooling does it scan
// forward for the next non-cooling key and adopt that as the new current.
//
// With no key pool it falls back to apiKey (idx -1, ok always true — this is
// not the exhausted case). If a pool exists but every key in it is cooling,
// ok is false: the provider is exhausted and the caller must not attempt a
// request with a known-cooling key.
func (a *activeProvider) pickKey() (key string, idx int, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := len(a.keys)
	if n == 0 {
		return a.apiKey, -1, true
	}
	now := time.Now()
	cur := int(a.rr) % n
	if a.cool[cur].Before(now) {
		return a.keys[cur], cur, true
	}
	for off := 1; off < n; off++ {
		i := (cur + off) % n
		if a.cool[i].Before(now) {
			a.rr = uint32(i)
			return a.keys[i], i, true
		}
	}
	return "", -1, false
}

// hasAvailableKey reports whether the provider currently has at least one
// non-cooling key, without mutating the sticky selection state. An empty
// key pool (fallback-to-apiKey case) always counts as available.
func (a *activeProvider) hasAvailableKey() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.keys) == 0 {
		return true
	}
	now := time.Now()
	for _, c := range a.cool {
		if c.Before(now) {
			return true
		}
	}
	return false
}

// penalize puts a key on cooldown after a rate-limit response and records d
// as its current backoff, so a later recovery-probe retry (see reschedule)
// knows what to double from. This stays the single place that marks a key
// cooling.
func (a *activeProvider) penalize(idx int, d time.Duration) {
	if idx < 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if idx < len(a.cool) {
		a.cool[idx] = time.Now().Add(d)
	}
	if idx < len(a.backoff) {
		a.backoff[idx] = d
	}
}

// clearCooldown resets a key's cooldown/backoff after a successful recovery
// probe: it's immediately eligible for sticky selection again, and starts
// fresh at initialKeyBackoff on its next 429.
func (a *activeProvider) clearCooldown(idx int) {
	if idx < 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if idx < len(a.cool) {
		a.cool[idx] = time.Time{}
	}
	if idx < len(a.backoff) {
		a.backoff[idx] = 0
	}
}

// reschedule doubles a cooling key's backoff (capped at maxKeyBackoff) after
// a recovery probe finds it still failing, and pushes its cooldown deadline
// out by the new backoff.
func (a *activeProvider) reschedule(idx int) {
	if idx < 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if idx >= len(a.backoff) {
		return
	}
	next := a.backoff[idx] * 2
	if next <= 0 || next > maxKeyBackoff {
		next = maxKeyBackoff
	}
	a.backoff[idx] = next
	if idx < len(a.cool) {
		a.cool[idx] = time.Now().Add(next)
	}
}

// dueKeys returns the indices of keys that currently carry a cooldown
// (backoff[idx] != 0, i.e. penalized and not yet recovered) whose deadline
// has passed — keys due for a recovery probe. A key that was never penalized
// (or was already cleared) is never "due".
func (a *activeProvider) dueKeys(now time.Time) []int {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []int
	for i := range a.cool {
		if i >= len(a.backoff) || a.backoff[i] == 0 {
			continue
		}
		if !a.cool[i].After(now) {
			out = append(out, i)
		}
	}
	return out
}

// Handler handles incoming Claude Code requests.
type Handler struct {
	httpClient  *http.Client
	router      *router.Router
	providers   map[string]*activeProvider
	db          *storage.DB
	broker      EventPublisher // may be nil
	budget      *budgetTracker
	cache       *responseCache // may be nil (disabled)
	cascade     bool           // cheap-first cascade with verification
	directModel bool           // StrategyDirect: forward the requested model id to OpenAI-compatible providers unchanged (no model_map/vision_model override)
	firewall    *redactor      // privacy firewall (nil = off)
	inspect     bool           // capture full prompt/response for the inspector
	rules       []config.Rule  // declarative routing overrides
	maxReqUSD   float64        // guardrail: downgrade a single request above this
	stopHealth  chan struct{}

	stickyMu sync.Mutex
	// sticky maps a requested Claude model string (e.g. "claude-sonnet-4-6")
	// to the provider name that most recently served it successfully. See
	// stickyReorder / stickyProvider / setSticky below.
	sticky map[string]string
}

// capText caps a captured prompt/response so the inspector can't bloat the DB.
func capText(s string) string {
	const max = 64 * 1024
	if len(s) > max {
		return s[:max] + "…[truncated]"
	}
	return s
}

// capForLog caps a raw request body for inclusion in an error log line.
func capForLog(b []byte) string {
	const max = 2 * 1024
	if len(b) > max {
		return string(b[:max]) + "…[truncated]"
	}
	return string(b)
}

// NewHandler builds the provider set + router from config and wires in the
// shared storage and (optional) event broker.
func NewHandler(cfg *Config, db *storage.DB, broker EventPublisher) (*Handler, error) {
	appCfg, err := config.Load(cfg.ConfigPath)
	if err != nil {
		return nil, err
	}

	// Zero-config boost: pick up provider keys already in the environment
	// (GROQ_API_KEY, OPENAI_API_KEY, …) that aren't explicitly configured.
	if disc := config.DiscoverFromEnv(appCfg.Providers); len(disc) > 0 {
		for _, d := range disc {
			log.Info().Str("provider", d.Name).Msg("Auto-discovered provider from environment")
		}
		appCfg.Providers = append(appCfg.Providers, disc...)
	}

	rt := router.New(router.RoutingStrategy(appCfg.Routing.Strategy))
	active := make(map[string]*activeProvider)
	for _, pc := range appCfg.Providers {
		keys := resolveProviderKeys(pc)
		key := keys[0]
		impl, err := providers.New(providers.Spec{
			Name:               pc.Name,
			Type:               pc.Type,
			APIKey:             key,
			BaseURL:            pc.BaseURL,
			Models:             pc.Models,
			Tier:               pc.Tier,
			ModelMap:           pc.ModelMap,
			VisionModel:        pc.VisionModel,
			InputPer1M:         pc.InputPer1M,
			OutputPer1M:        pc.OutputPer1M,
			OffPeakInputPer1M:  pc.OffPeakInputPer1M,
			OffPeakOutputPer1M: pc.OffPeakOutputPer1M,
			OffPeakStartUTC:    pc.OffPeakStartUTC,
			OffPeakEndUTC:      pc.OffPeakEndUTC,
			Region:             pc.Region,
			Project:            pc.Project,
			APIVersion:         pc.APIVersion,
		})
		if err != nil {
			log.Warn().Str("provider", pc.Name).Err(err).Msg("Skipping provider")
			continue
		}
		active[impl.Name()] = &activeProvider{impl: impl, apiKey: key, keys: keys, cool: make([]time.Time, len(keys)), backoff: make([]time.Duration, len(keys))}
		rt.AddProvider(&router.Provider{
			Name:    impl.Name(),
			BaseURL: impl.BaseURL(),
			APIKey:  key,
			Tier:    impl.Tier(),
			Pricing: router.Pricing{InputPer1M: impl.Pricing().InputPer1M, OutputPer1M: impl.Pricing().OutputPer1M},
			Healthy: true, // optimistic; runtime failover handles outages/rate-limits
		})
	}

	budgetLimit := cfg.DailyBudgetUSD
	if budgetLimit <= 0 {
		budgetLimit = appCfg.Routing.DailyBudgetUSD
	}
	var spentToday float64
	if s, err := db.GetStats("today"); err == nil {
		spentToday = s.TotalCostUSD
	}
	alertWebhook := cfg.AlertWebhook
	if alertWebhook == "" {
		alertWebhook = appCfg.Routing.AlertWebhook
	}
	alertThreshold := cfg.AlertThreshold
	if alertThreshold <= 0 {
		alertThreshold = appCfg.Routing.AlertThreshold
	}

	h := &Handler{
		httpClient: &http.Client{Timeout: 5 * time.Minute},
		router:     rt,
		providers:  active,
		db:         db,
		broker:     broker,
		budget:     newBudgetTracker(budgetLimit, spentToday, alertWebhook, alertThreshold),
		stopHealth: make(chan struct{}),
	}
	if !cfg.DisableCache {
		semantic := cfg.SemanticCache || appCfg.Routing.SemanticCache
		threshold := cfg.SemanticThreshold
		if threshold <= 0 {
			threshold = appCfg.Routing.SemanticThreshold
		}
		h.cache = newResponseCache(5*time.Minute, 500, semantic, threshold)
		log.Info().Msg("Response cache enabled (5m TTL) — identical requests served instantly & free")
		if semantic {
			log.Info().Float64("threshold", h.cache.threshold).Msg("Semantic cache enabled — near-identical tool-less requests served from cache")
		}
	}

	h.cascade = cfg.Cascade || appCfg.Routing.Cascade
	h.directModel = router.RoutingStrategy(appCfg.Routing.Strategy) == router.StrategyDirect
	if cfg.Adaptive || appCfg.Routing.Adaptive {
		rt.SetAdaptive(true)
		log.Info().Msg("Adaptive routing enabled — NEXUS learns the best provider per task type")
	}
	if cfg.Redact || appCfg.Routing.Redact {
		h.firewall = &redactor{}
		log.Info().Msg("Privacy firewall enabled — secrets/PII are masked before leaving for any provider")
	}
	h.inspect = cfg.Inspect || appCfg.Routing.Inspect
	if h.inspect {
		log.Info().Msg("Request inspector enabled — full prompts/responses are stored locally for replay")
	}
	h.rules = appCfg.Rules
	if len(h.rules) > 0 {
		log.Info().Int("rules", len(h.rules)).Msg("Routing rules loaded")
	}
	h.maxReqUSD = cfg.MaxRequestUSD
	if h.maxReqUSD <= 0 {
		h.maxReqUSD = appCfg.Routing.MaxRequestUSD
	}
	if h.maxReqUSD > 0 {
		log.Info().Float64("max_request_usd", h.maxReqUSD).Msg("Cost guardrail enabled — pricey single requests are downgraded to free/local")
	}

	if len(active) == 0 {
		log.Info().Msg("No providers configured — zero-config mode (forwarding directly to Anthropic)")
	} else {
		log.Info().Int("providers", len(active)).Str("strategy", appCfg.Routing.Strategy).Msg("Router configured")
		if budgetLimit > 0 {
			log.Info().Float64("daily_budget_usd", budgetLimit).Msg("Daily budget cap enabled — free/local only once exceeded")
		}
		if alertWebhook != "" {
			log.Info().Msg("Budget alerts enabled — webhook fires at threshold and when exceeded")
		}
		if h.cascade {
			log.Info().Msg("Cheap-first cascade enabled — try the cheapest capable model, verify, escalate on failure")
		}
		if h.directModel {
			log.Info().Msg("Direct strategy enabled — requested model id is forwarded to OpenAI-compatible providers unchanged (model_map/vision_model overrides are bypassed)")
		}
		go h.keyProbeLoop(h.stopHealth) // background recovery probing of cooling keys
	}
	return h, nil
}

// Close stops background work. The shared DB is owned and closed by the caller.
func (h *Handler) Close() error {
	if h.stopHealth != nil {
		close(h.stopHealth)
		h.stopHealth = nil
	}
	return nil
}

// ProviderCount returns the number of configured providers.
func (h *Handler) ProviderCount() int { return len(h.providers) }

// CacheEnabled reports whether the response cache is active.
func (h *Handler) CacheEnabled() bool { return h.cache != nil }

// probeTickInterval is how often keyProbeLoop scans for cooling keys whose
// own backoff deadline has passed. It's much finer than the probes it
// triggers — per-key backoff (initialKeyBackoff..maxKeyBackoff) is what
// actually rate-limits the outbound probe requests, this just bounds how
// promptly a newly-due key gets picked up.
const probeTickInterval = 5 * time.Second

// probeModel is the representative Claude model name used to build the
// synthetic recovery-probe request below. Every provider's MapModel /
// AnthropicNative path accepts any Claude Code model string, so one constant
// works uniformly across providers — the probe only cares whether the key
// itself is accepted, not which model tier answers it.
const probeModel = "claude-haiku-4-5"

// probeRequest and probeRequestBody are the synthetic minimal chat-completion
// request keyProbeLoop fires at a cooling key to test whether it has actually
// recovered. Built once since the payload never varies.
var probeRequest = AnthropicRequest{
	Model:     probeModel,
	MaxTokens: 1,
	Messages:  []Message{{Role: "user", Content: "ping"}},
}

var probeRequestBody = func() []byte {
	b, err := json.Marshal(probeRequest)
	if err != nil {
		panic(fmt.Sprintf("nexus: failed to marshal static probe request: %v", err))
	}
	return b
}()

// keyProbeLoop replaces the old generic healthLoop. Instead of pinging a
// /models-style endpoint on each provider's primary key on a fixed interval —
// a false signal, since that endpoint can respond fine while the actual
// chat-completions endpoint a key was 429'd on is still rate-limited — it
// scans every provider's cooling keys and fires a real minimal
// chat-completion request at each one once its own backoff deadline has
// passed. router.Provider.Healthy is a derived fact about key state (see
// coolKey/recoverKey), not an independent signal fed by this loop directly.
//
// stop is passed in (captured by the caller before spawning this goroutine)
// rather than read from h.stopHealth here, so Close()'s unsynchronized
// `h.stopHealth = nil` can never race with this loop's own read of the field.
func (h *Handler) keyProbeLoop(stop <-chan struct{}) {
	ticker := time.NewTicker(probeTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			h.probeCoolingKeys()
		}
	}
}

// probeCoolingKeys does one pass over every configured provider, probing each
// of its cooling keys whose backoff deadline has passed. Split out from
// keyProbeLoop so tests can call it directly without waiting on a real ticker.
func (h *Handler) probeCoolingKeys() {
	now := time.Now()
	var wg sync.WaitGroup
	for name, active := range h.providers {
		due := active.dueKeys(now)
		if len(due) == 0 {
			continue
		}
		wg.Add(1)
		go func(name string, active *activeProvider, due []int) {
			defer wg.Done()
			for _, idx := range due {
				if idx < 0 || idx >= len(active.keys) {
					continue
				}
				h.probeKey(active, name, idx, active.keys[idx])
			}
		}(name, active, due)
	}
	wg.Wait()
}

// probeKey fires one minimal real chat-completion request at a specific
// cooling key via the same request-building path real traffic uses
// (callUpstreamOnce), so recovery is verified against the actual endpoint the
// key was rate-limited on. A non-429 response (or, symmetrically, any
// response at all — a transport error is treated the same as "still no
// evidence of recovery" as a 429) decides the outcome: non-429 clears the
// key's cooldown, anything else doubles its backoff and reschedules the next
// probe. Every probe is a real upstream attempt, so each outcome (transport
// error, 429, recovery) records a probe-flagged usage event; probes are
// excluded from quota windows/totals by default but stay queryable.
func (h *Handler) probeKey(active *activeProvider, name string, idx int, key string) {
	start := time.Now()
	resp, err := h.callUpstreamOnce(active, probeRequest, probeRequestBody, http.Header{}, key)
	if err != nil {
		if !errors.Is(err, errLocalPrep) {
			h.recordUsageEvent(active, probeRequest, false, 0, &attemptInfo{
				keyIdx: idx, chainPos: 0, started: start, probe: true,
				errText: "transport: " + err.Error(),
			}, rawUsage{})
		}
		active.reschedule(idx)
		return
	}
	probeBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	att := &attemptInfo{
		keyIdx: idx, chainPos: 0, started: start, probe: true,
		quota: captureQuota(resp.Header), reqID: extractRequestID(resp.Header),
	}
	raw := usageFromRawBody(active.impl.Name(), probeBody)
	if resp.StatusCode == http.StatusTooManyRequests {
		h.recordUsageEvent(active, probeRequest, false, resp.StatusCode, att, raw)
		active.reschedule(idx)
		return
	}
	h.recordUsageEvent(active, probeRequest, false, resp.StatusCode, att, raw)
	h.recoverKey(active, name, idx)
}

// routerHealthy reports a provider's currently-recorded router.Provider.Healthy
// flag — the actual gate RouteChain filters on. This is deliberately not the
// same thing as active.hasAvailableKey(): that recomputes live off wall-clock
// time, so a key whose backoff deadline has just elapsed already reads as
// "available" before a recovery probe has confirmed anything, whereas
// router.Healthy only ever changes via an explicit SetHealthy call. coolKey/
// recoverKey below need the latter to detect a genuine transition.
func (h *Handler) routerHealthy(name string) bool {
	for _, p := range h.router.Providers() {
		if p.Name == name {
			return p.Healthy
		}
	}
	return false
}

// coolKey marks key idx on active as cooling and, if the provider is
// router-healthy but is now out of available keys, flips its router entry
// unhealthy — router.Healthy is a computed fact about the key pool, not an
// independently-driven signal.
func (h *Handler) coolKey(active *activeProvider, name string, idx int, d time.Duration) {
	active.penalize(idx, d)
	if h.routerHealthy(name) && !active.hasAvailableKey() {
		h.router.SetHealthy(name, false)
	}
}

// recoverKey clears key idx's cooldown after a successful recovery probe and,
// if the provider is currently router-unhealthy but now has an available key,
// flips it back healthy.
func (h *Handler) recoverKey(active *activeProvider, name string, idx int) {
	active.clearCooldown(idx)
	if !h.routerHealthy(name) && active.hasAvailableKey() {
		h.router.SetHealthy(name, true)
	}
}

// stickyProvider returns the provider name last recorded (via setSticky) to
// have successfully served requestedModel, if any.
func (h *Handler) stickyProvider(requestedModel string) (string, bool) {
	h.stickyMu.Lock()
	defer h.stickyMu.Unlock()
	name, ok := h.sticky[requestedModel]
	return name, ok
}

// setSticky records provider as the provider that most recently succeeded in
// serving requestedModel, so subsequent requests for the same requested
// model prefer it (see stickyReorder) until it stops working.
func (h *Handler) setSticky(requestedModel, provider string) {
	h.stickyMu.Lock()
	defer h.stickyMu.Unlock()
	if h.sticky == nil {
		h.sticky = make(map[string]string)
	}
	h.sticky[requestedModel] = provider
}

// stickyReorder moves the chain entry named sticky (if present) to the
// front, preserving the relative order of the rest — it never bypasses
// RouteChain's own eligibility/ordering decision, only reprioritizes what
// RouteChain already returned for this one request. If sticky is empty or
// not found in chain (it fell out of availability, or nothing's been
// recorded yet), chain is returned unchanged. Pure function, easy to test
// without a full Handler.
func stickyReorder(chain []*router.Provider, sticky string) []*router.Provider {
	if sticky == "" {
		return chain
	}
	idx := -1
	for i, p := range chain {
		if p.Name == sticky {
			idx = i
			break
		}
	}
	if idx <= 0 { // not found, or already at the front — nothing to do
		return chain
	}
	out := make([]*router.Provider, 0, len(chain))
	out = append(out, chain[idx])
	out = append(out, chain[:idx]...)
	out = append(out, chain[idx+1:]...)
	return out
}

// HandleMessages is the main handler for POST /v1/messages (Claude Code calls this).
func (h *Handler) HandleMessages(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	defer r.Body.Close()

	user := deriveUser(r.Header) // team attribution

	// Privacy firewall: mask secrets/PII before anything downstream (cache key,
	// classification, upstream) sees the body. Originals are restored in the
	// response by a restoringWriter wrapped around w below.
	var restoreMap map[string]string
	if h.firewall != nil {
		if red, m := h.firewall.redact(body); len(m) > 0 {
			body, restoreMap = red, m
		}
	}

	// Response cache: serve identical requests instantly (and free).
	if h.cache != nil {
		key := cacheKey("m", body)
		if e, ok := h.cache.get(key); ok {
			h.serveCached(w, e, startTime, user)
			return
		}
		var vec sparseVec
		hasTools := false
		if h.cache.semantic {
			if text, ht, hi, ok := promptText(body); ok {
				hasTools = ht
				if !ht && !hi {
					vec = embed(text)
					if e, ok := h.cache.getSemantic(quickModel(body), vec); ok {
						h.serveCached(w, e, startTime, user)
						return
					}
				}
			}
		}
		cw := newCachingWriter(w)
		defer func() {
			if cw.cacheable() {
				e := cw.entry()
				e.model = quickModel(body)
				e.vec = vec
				e.hasTools = hasTools
				h.cache.set(key, e)
			}
		}()
		w = cw
	}

	// Restore masked secrets/PII in the response (outermost wrapper so the cache
	// stores the restored bytes too).
	if restoreMap != nil {
		rw := newRestoringWriter(w, restoreMap)
		defer rw.flush()
		w = rw
	}

	var req AnthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		log.Error().Err(err).Str("body", capForLog(body)).Msg("Failed to parse request body as JSON")
		h.writeError(w, http.StatusBadRequest, "invalid JSON in request body")
		return
	}
	req.nexusUser = user
	req.nexusRedacted = len(restoreMap)

	// Parse messages as raw maps for the classifier.
	var raw struct {
		Messages []map[string]interface{} `json:"messages"`
	}
	_ = json.Unmarshal(body, &raw)
	hasTools := len(req.Tools) > 0
	req.nexusImages = messagesHaveImage(raw.Messages)
	complexity := router.ClassifyRequest(req.Model, raw.Messages, hasTools)

	log.Debug().
		Str("model", req.Model).
		Str("complexity", complexity.String()).
		Int("messages", len(req.Messages)).
		Bool("stream", req.Stream).
		Bool("tools", hasTools).
		Msg("Incoming request")

	// Zero-config (no providers) or empty chain → forward straight to Anthropic.
	chain := h.router.RouteChain(req.Model, complexity)
	if len(h.providers) == 0 || len(chain) == 0 {
		h.forwardDirectAnthropic(w, r, req, body, startTime, complexity)
		return
	}

	// Sticky provider (P3): prefer whichever provider last actually
	// succeeded serving this exact requested model string, if it's still in
	// this request's chain. NOTE: under the default StrategyAuto, RouteChain
	// reorders purely by classified complexity and ignores the requested
	// model string, so the same requested model can land in a different
	// chain (and this sticky pointer simply won't be found) from one
	// request to the next if its classified complexity differs — that's
	// intentional graceful degradation, not a bug to work around here.
	if sp, ok := h.stickyProvider(req.Model); ok {
		chain = stickyReorder(chain, sp)
	}

	// Routing rules + cost guardrail need the prompt text.
	var ptext string
	if len(h.rules) > 0 || h.maxReqUSD > 0 {
		ptext, _, _, _ = promptText(body)
	}

	// Explicit provider pin (header), then config rules (provider or tier).
	forced := r.Header.Get("X-Nexus-Provider")
	headerTier := r.Header.Get("X-Nexus-Tier")
	bypassCascade := false

	// Config rules apply only when no explicit per-request header override is set.
	if forced == "" && headerTier == "" && len(h.rules) > 0 {
		if rp, rt := applyRules(h.rules, req.Model, ptext, complexity, hasTools); rp != "" {
			if _, ok := h.providers[rp]; ok {
				forced = rp
			}
		} else if rt != "" {
			chain = filterByTier(chain, rt)
			bypassCascade = true
		}
	}
	if forced != "" {
		if _, ok := h.providers[forced]; !ok {
			h.writeError(w, http.StatusBadRequest, "X-Nexus-Provider: unknown provider "+forced)
			return
		}
		chain = []*router.Provider{{Name: forced}}
		bypassCascade = true
	} else if headerTier != "" {
		// Per-request tier pin — lets an agent harness (e.g. ECC) route a skill:
		// X-Nexus-Tier: premium for architecture, free for lint/format, …
		chain = filterByTier(chain, headerTier)
		bypassCascade = true
	}

	// Cost guardrail: downgrade a single request estimated to exceed the cap.
	if !bypassCascade && h.maxReqUSD > 0 && len(chain) > 0 {
		if head := h.providers[chain[0].Name]; head != nil {
			if est := head.impl.Pricing().CalculateCost(estimateTokens(ptext), req.MaxTokens); est > h.maxReqUSD {
				if cheap := freeLocalOnly(chain); len(cheap) > 0 {
					log.Warn().Float64("est_usd", est).Float64("cap", h.maxReqUSD).Msg("Cost guardrail: downgrading to free/local")
					chain = cheap
					bypassCascade = true
				}
			}
		}
	}

	// Cheap-first cascade: try the cheapest capable provider, verify its output,
	// and escalate to a stronger model only on failure. Falls through to the
	// normal failover path if every cascade candidate is unreachable.
	if h.cascade && !bypassCascade {
		cc := h.router.CascadeChain(complexity)
		if h.budget.Over() {
			if cheap := freeLocalOnly(cc); len(cheap) > 0 {
				cc = cheap
			}
		}
		if len(cc) > 0 && h.serveCascade(w, r, req, body, startTime, complexity, cc) {
			return
		}
	}

	// Daily budget cap: once today's spend exceeds the limit, restrict to
	// free/local providers (paid tiers are skipped until the next day).
	if forced == "" && h.budget.Over() {
		if cheap := freeLocalOnly(chain); len(cheap) > 0 {
			chain = cheap
		} else {
			log.Warn().Msg("Daily budget exceeded but no free/local provider available — using a paid provider")
		}
	}

	// Walk the chain, failing over to the next provider on transport errors and
	// on retryable HTTP statuses (rate-limit / server errors). A provider's own
	// 4xx (e.g. 401 bad key) is relayed to the client as-is.
	for i, cand := range chain {
		active := h.providers[cand.Name]
		if active == nil {
			continue
		}

		// Up to maxProviderAttempts total attempts against this one chain
		// entry before treating it as failed for this request and falling
		// through to the next-provider failover below. Only non-429
		// retryable statuses (500/502/503/504) and transport errors are
		// retried here, with no backoff — a tight retry, same sticky key
		// each time since callUpstream/pickKey keep returning it as long as
		// it isn't cooling. 429 is untouched: it breaks out immediately and
		// is handled by the unchanged failover logic right below, since a
		// 429 reaching here already means callUpstream's own same-provider
		// key rotation (P1) is exhausted.
		var resp *http.Response
		var att *attemptInfo
		var err error
		for attempt := 1; attempt <= maxProviderAttempts; attempt++ {
			resp, att, err = h.callUpstream(active, req, body, r.Header, i+1)
			if err == nil && (resp.StatusCode == http.StatusTooManyRequests || !isRetryableStatus(resp.StatusCode)) {
				break
			}
			if attempt == maxProviderAttempts {
				break
			}
			if err != nil {
				// transport error: the event was already recorded inside callUpstream
				log.Warn().Str("provider", cand.Name).Err(err).Int("attempt", attempt).Msg("Provider request failed, retrying same provider")
			} else {
				// this response is being discarded for a tight retry — the
				// client never sees it, so record its attempt here
				h.recordUsageEvent(active, req, req.Stream, resp.StatusCode, att, rawUsage{})
				log.Warn().Str("provider", cand.Name).Int("status", resp.StatusCode).Int("attempt", attempt).Msg("Retryable error, retrying same provider")
				resp.Body.Close()
			}
		}

		if err != nil {
			log.Warn().Str("provider", cand.Name).Err(err).Msg("Provider unreachable, trying next")
			continue
		}
		if isRetryableStatus(resp.StatusCode) && i < len(chain)-1 {
			h.router.RecordOutcome(cand.Name, complexity, false)
			// the last discarded response of the retry loop — recorded before
			// the failover drops it
			h.recordUsageEvent(active, req, req.Stream, resp.StatusCode, att, rawUsage{})
			resp.Body.Close()
			log.Warn().Str("provider", cand.Name).Int("status", resp.StatusCode).Msg("Retryable error, failing over to next provider")
			continue
		}
		succeeded := resp.StatusCode < 400
		h.router.RecordOutcome(cand.Name, complexity, succeeded)
		if succeeded {
			// P3: this is the provider the walk actually succeeded on —
			// whether on the first try, after a 429-driven key/provider
			// switch, or after this retry-then-failover path — so pin
			// subsequent requests for this requested model to it.
			h.setSticky(req.Model, cand.Name)
		}
		switch {
		case providers.IsOpenAICompatible(active.impl.Name()) && req.Stream:
			h.relayOpenAIStream(w, r, active, req, resp, startTime, complexity, att)
		case providers.IsOpenAICompatible(active.impl.Name()):
			h.relayOpenAI(w, active, req, resp, startTime, complexity, att)
		default:
			// Anthropic-format. Bedrock/Vertex return a full body (buffered);
			// native Anthropic streams through.
			if _, custom := active.impl.(providers.AnthropicNative); custom {
				h.relayAnthropicBuffered(w, active, req, resp, startTime, complexity, att)
			} else if req.Stream {
				h.relayAnthropicStream(w, r, active, req, resp, startTime, complexity, att)
			} else {
				h.relayAnthropicSync(w, active, req, resp, startTime, complexity, att)
			}
		}
		return
	}

	h.writeError(w, http.StatusBadGateway, "all providers unreachable")
}

// resolveProviderKeys returns the resolved key pool for a provider: api_keys if
// present, else the single api_key (always ≥1 element, possibly "").
func resolveProviderKeys(pc config.Provider) []string {
	var out []string
	for _, k := range pc.APIKeys {
		out = append(out, config.ResolveKey(k))
	}
	if len(out) == 0 {
		out = append(out, config.ResolveKey(pc.APIKey))
	}
	return out
}

// errProviderExhausted is returned by callUpstream when a provider has a key
// pool but every key in it is currently cooling — i.e. there is no key left
// to even attempt a request with. Callers already treat any non-nil error
// from callUpstream as "this provider is unusable, try the next one in the
// chain" (see HandleMessages/HandleChatCompletions/serveCascade), so this
// sentinel needs no special-cased handling there; it's exposed so a later
// caller can distinguish "provider exhausted" from a transport error with
// errors.Is if it needs to.
var errProviderExhausted = errors.New("nexus: provider exhausted, all keys cooling")

// errLocalPrep wraps failures that happen before anything leaves the machine
// (transform/marshal/NewRequest errors inside callUpstreamOnce and
// callOpenAIPassthrough). They consumed nothing upstream, so callUpstream must
// NOT record a usage event for them — only real round-trip attempts do.
var errLocalPrep = errors.New("nexus: local request preparation failed")

// callUpstream issues the upstream HTTP request, sticking to one API key from
// the provider's pool: on a 429 it cools that key and moves to the next
// non-cooling one, so the handler only fails over to a different provider
// once every key for this provider is rate-limited. It returns a transport
// error only — provider HTTP errors come back in *http.Response.
//
// The returned attemptInfo describes the winning attempt (the response the
// caller will relay); key-rotation 429s and transport errors are recorded as
// usage events here because no caller ever sees them. chainPos is the 1-based
// position of this provider in the caller's chain, stamped onto every event
// (rotations and retries of the same chain entry share its position).
func (h *Handler) callUpstream(active *activeProvider, req AnthropicRequest, body []byte, origHeaders http.Header, chainPos int) (*http.Response, *attemptInfo, error) {
	attempts := len(active.keys)
	if attempts < 1 {
		attempts = 1
	}
	var resp *http.Response
	var err error
	for i := 0; i < attempts; i++ {
		key, idx, ok := active.pickKey()
		if !ok {
			return nil, nil, errProviderExhausted
		}
		start := time.Now()
		resp, err = h.callUpstreamOnce(active, req, body, origHeaders, key)
		if err != nil {
			if !errors.Is(err, errLocalPrep) {
				h.recordUsageEvent(active, req, req.Stream, 0, &attemptInfo{
					keyIdx: idx, chainPos: chainPos, started: start,
					errText: "transport: " + err.Error(),
				}, rawUsage{})
			}
			return resp, nil, err
		}
		if resp.StatusCode == http.StatusTooManyRequests && i < attempts-1 {
			// This 429 never reaches the client — record it before the body
			// closes, transcribing the rate-limit headers it carried.
			att := &attemptInfo{
				keyIdx: idx, chainPos: chainPos, started: start,
				quota: captureQuota(resp.Header), reqID: extractRequestID(resp.Header),
			}
			h.coolKey(active, active.impl.Name(), idx, initialKeyBackoff)
			resp.Body.Close()
			h.recordUsageEvent(active, req, req.Stream, resp.StatusCode, att, rawUsage{})
			log.Warn().Str("provider", active.impl.Name()).Msg("key rate-limited (429), rotating to next key")
			continue
		}
		return resp, &attemptInfo{
			keyIdx: idx, chainPos: chainPos, started: start,
			quota: captureQuota(resp.Header), reqID: extractRequestID(resp.Header),
		}, nil
	}
	return resp, nil, err
}

// mappedModel resolves the provider-facing model id for logging/dashboard
// records and for the OpenAI-compatible gateway's raw pass-through path:
// under StrategyDirect the client's requested model is returned verbatim
// (bypassing ModelMap); otherwise it's the provider's ordinary MapModel
// result. It deliberately does NOT apply a vision_model override — use
// upstreamModel for the model id that is actually sent upstream.
func (h *Handler) mappedModel(active *activeProvider, requestedModel string) string {
	if h.directModel {
		return requestedModel
	}
	return active.impl.MapModel(requestedModel)
}

// upstreamModel is the single source of truth for the model id an
// OpenAI-compatible provider is addressed with, shared by the /v1/messages
// path (callUpstreamOnce), the /v1/chat/completions pass-through, and every
// log/usage record, so the recorded model_used always names the model that
// was really sent. Order: StrategyDirect returns the requested id verbatim
// (no model_map, no vision_model, images or not); otherwise an image-bearing
// request uses the provider's operator-configured vision_model when set;
// otherwise the ordinary mapped model. The vision override only applies to
// OpenAI-compatible providers — the Anthropic-native paths forward the model
// id unchanged and never consult vision_model.
func (h *Handler) upstreamModel(active *activeProvider, requestedModel string, images bool) string {
	if h.directModel {
		return requestedModel
	}
	if images && providers.IsOpenAICompatible(active.impl.Name()) {
		if vc, ok := active.impl.(providers.VisionCapable); ok {
			if vm := vc.VisionModel(requestedModel); vm != "" {
				return vm
			}
		}
	}
	return h.mappedModel(active, requestedModel)
}

// callUpstreamOnce performs a single upstream request with a specific API key.
func (h *Handler) callUpstreamOnce(active *activeProvider, req AnthropicRequest, body []byte, origHeaders http.Header, key string) (*http.Response, error) {
	if providers.IsOpenAICompatible(active.impl.Name()) {
		oaiReq, err := TransformToOpenAI(req, h.upstreamModel(active, req.Model, req.nexusImages))
		if err != nil {
			return nil, fmt.Errorf("%w: request transform failed: %v", errLocalPrep, err)
		}
		oaiReq.Stream = req.Stream // stream upstream when the client streams
		if req.Stream {
			oaiReq.StreamOptions = &OpenAIStreamOptions{IncludeUsage: true}
		}
		payload, err := json.Marshal(oaiReq)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errLocalPrep, err)
		}
		httpReq, err := http.NewRequest("POST", active.impl.ChatCompletionsURL(), bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errLocalPrep, err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		h.authorize(active, httpReq, payload, key)
		return h.httpClient.Do(httpReq)
	}

	// Anthropic-format providers (Anthropic, plus Bedrock/Vertex via AnthropicNative).
	url := active.impl.BaseURL() + "/v1/messages"
	sendBody := body
	if an, ok := active.impl.(providers.AnthropicNative); ok {
		url = an.MessagesURL(req.Model)
		sendBody = an.PrepareBody(body, req.Model)
	}
	httpReq, err := http.NewRequest("POST", url, bytes.NewReader(sendBody))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errLocalPrep, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if _, ok := active.impl.(providers.Authorizer); ok {
		h.authorize(active, httpReq, sendBody, key)
	} else {
		httpReq.Header.Set("x-api-key", resolveAnthropicKeyFor(key, origHeaders))
		httpReq.Header.Set("anthropic-version", "2023-06-01")
		if v := origHeaders.Get("anthropic-beta"); v != "" {
			httpReq.Header.Set("anthropic-beta", v)
		}
		if v := origHeaders.Get("anthropic-version"); v != "" {
			httpReq.Header.Set("anthropic-version", v)
		}
	}
	return h.httpClient.Do(httpReq)
}

// authorize applies a provider's custom auth (Azure api-key, Vertex bearer,
// Bedrock SigV4) when it implements Authorizer; otherwise falls back to Bearer.
func (h *Handler) authorize(active *activeProvider, req *http.Request, body []byte, key string) {
	if az, ok := active.impl.(providers.Authorizer); ok {
		az.Authorize(req, body, key)
		return
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
}

// forwardDirectAnthropic is the zero-config path: forward to Anthropic using the
// client's (or the server env's) key, exactly like Sprint 1.
func (h *Handler) forwardDirectAnthropic(w http.ResponseWriter, r *http.Request, req AnthropicRequest, body []byte, startTime time.Time, complexity router.Complexity) {
	active := &activeProvider{impl: providers.NewAnthropic(""), apiKey: ""}
	resp, att, err := h.callUpstream(active, req, body, r.Header, 1)
	if err != nil {
		log.Error().Err(err).Msg("Provider request failed")
		h.writeError(w, http.StatusBadGateway, fmt.Sprintf("provider error: %v", err))
		return
	}
	if req.Stream {
		h.relayAnthropicStream(w, r, active, req, resp, startTime, complexity, att)
	} else {
		h.relayAnthropicSync(w, active, req, resp, startTime, complexity, att)
	}
}

// relayAnthropicSync relays a non-streaming native-Anthropic response.
func (h *Handler) relayAnthropicSync(w http.ResponseWriter, active *activeProvider, req AnthropicRequest, resp *http.Response, startTime time.Time, complexity router.Complexity, att *attemptInfo) {
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "failed to read provider response")
		return
	}

	copyResponseHeaders(w.Header(), resp.Header)
	w.Header().Set("X-Nexus-Provider", active.impl.Name())
	w.Header().Set("X-Nexus-Latency", fmt.Sprintf("%dms", time.Since(startTime).Milliseconds()))
	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(respBody); err != nil {
		log.Warn().Err(err).Msg("Failed to write response to client")
	}

	raw := anthropicRawUsage(respBody)
	u := raw.anthropicTokens()
	h.logResult(active, req, complexity, u, respBody, resp.StatusCode, time.Since(startTime), false, att, raw)
	log.Info().
		Str("provider", active.impl.Name()).
		Int("status", resp.StatusCode).
		Int("cache_read", u.CacheRead).
		Int64("latency_ms", time.Since(startTime).Milliseconds()).
		Str("complexity", complexity.String()).
		Msg("Request completed")
}

// relayAnthropicBuffered handles Anthropic-format providers that return a full
// (non-streaming) body — Bedrock/Vertex. It relays the JSON, or synthesizes the
// Anthropic SSE sequence when the client asked to stream.
func (h *Handler) relayAnthropicBuffered(w http.ResponseWriter, active *activeProvider, req AnthropicRequest, resp *http.Response, startTime time.Time, complexity router.Complexity, att *attemptInfo) {
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "failed to read provider response")
		return
	}

	raw := anthropicRawUsage(respBody)
	if resp.StatusCode >= 400 {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Nexus-Provider", active.impl.Name())
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
		h.logResult(active, req, complexity, tokenUsage{}, respBody, resp.StatusCode, time.Since(startTime), req.Stream, att, raw)
		log.Warn().Str("provider", active.impl.Name()).Int("status", resp.StatusCode).Msg("Provider returned error")
		return
	}

	u := raw.anthropicTokens()
	if req.Stream {
		var ar AnthropicResponse
		if json.Unmarshal(respBody, &ar) == nil && len(ar.Content) > 0 {
			if ar.Model == "" {
				ar.Model = req.Model
			}
			writeAnthropicSSE(w, active.impl.Name(), ar)
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Nexus-Provider", active.impl.Name())
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(respBody)
		}
	} else {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Nexus-Provider", active.impl.Name())
		w.Header().Set("X-Nexus-Latency", fmt.Sprintf("%dms", time.Since(startTime).Milliseconds()))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(respBody)
	}

	h.logResult(active, req, complexity, u, respBody, http.StatusOK, time.Since(startTime), req.Stream, att, raw)
	log.Info().
		Str("provider", active.impl.Name()).
		Int("in", u.In).Int("out", u.Out).
		Int64("latency_ms", time.Since(startTime).Milliseconds()).
		Bool("stream", req.Stream).
		Msg("Request completed (anthropic-native)")
}

// resolveAnthropicKeyFor picks the API key for an Anthropic forward: the given
// configured key if present, otherwise the client's key, otherwise the server's
// env key.
func resolveAnthropicKeyFor(configured string, origHeaders http.Header) string {
	if configured != "" && configured != "nexus-local" {
		return configured
	}
	key := extractAPIKey(origHeaders)
	if key == "" || key == "nexus-local" {
		if env := os.Getenv("ANTHROPIC_API_KEY"); env != "" && env != "nexus-local" {
			key = env
		}
	}
	return key
}

// Abort annotations for streams that ended before completing. The same text
// lands on the usage event (error) and the requests row (Error column) so
// the two records tell one story.
const (
	abortClientGone  = "aborted: client disconnected"
	abortUpstreamErr = "aborted: upstream stream error"
)

// logAbortedStream records a stream that ended before completing — client
// disconnect or upstream read error — preserving partial-usage semantics: the
// tokens observed up to the abort are kept (the provider generated and billed
// them), usage_partial=1 marks the incompleteness, and the abort text marks
// both the usage event and the requests row.
func (h *Handler) logAbortedStream(active *activeProvider, req AnthropicRequest, complexity router.Complexity, u tokenUsage, respBody []byte, status int, startTime time.Time, att *attemptInfo, raw rawUsage, abortText string) {
	if att != nil {
		att.partial = true
		att.errText = abortText
	}
	h.logResult(active, req, complexity, u, respBody, status, time.Since(startTime), true, att, raw)
}

// logResult records a completed request to storage and pushes live events.
// respBody is the upstream response (used only for --inspect capture; may be nil).
// att is the winning upstream attempt's metadata (chain position, key slot,
// quota headers); raw is the presence-preserving usage report of that same
// response. Together they produce the immutable usage event.
func (h *Handler) logResult(active *activeProvider, req AnthropicRequest, complexity router.Complexity, u tokenUsage, respBody []byte, status int, latency time.Duration, stream bool, att *attemptInfo, raw rawUsage) {
	now := time.Now()
	pricing := active.impl.Pricing()
	cost := pricing.CalculateCostFullAt(u.In, u.Out, u.CacheRead, u.CacheWrite, now) // off-peak-aware
	cacheSaved := pricing.CacheReadSavings(u.CacheRead)
	h.budget.Add(cost)
	rec := &storage.Request{
		CreatedAt:        now,
		ModelAsked:       req.Model,
		ModelUsed:        h.upstreamModel(active, req.Model, req.nexusImages),
		Provider:         active.impl.Name(),
		Complexity:       complexity.String(),
		InputTokens:      u.In,
		OutputTokens:     u.Out,
		CacheReadTokens:  u.CacheRead,
		CacheWriteTokens: u.CacheWrite,
		CostUSD:          cost,
		CacheSavedUSD:    cacheSaved,
		LatencyMS:        latency.Milliseconds(),
		Status:           status,
		Stream:           stream,
		User:             req.nexusUser,
		Redacted:         req.nexusRedacted,
	}
	if att != nil && att.partial {
		// aborted stream: mark the requests row too (its Error column was
		// never populated before; aborted streams are its first user)
		rec.Error = att.errText
	}
	if h.inspect { // opt-in: capture full prompt + response for the inspector
		if pj, err := json.Marshal(req); err == nil {
			// elide base64 image payloads BEFORE capping, so the 64KB budget is
			// spent on text, not on truncated base64
			rec.Prompt = capText(elideImageData(string(pj)))
		}
		rec.Response = capText(string(respBody))
	}

	var id int64
	if h.db != nil {
		var err error
		if id, err = h.db.LogRequest(rec); err != nil {
			log.Warn().Err(err).Msg("Failed to log request")
		}
	}

	if h.broker != nil {
		h.broker.Publish("request", requestEvent{
			ID:            id,
			Provider:      rec.Provider,
			ModelAsked:    rec.ModelAsked,
			ModelUsed:     rec.ModelUsed,
			Complexity:    rec.Complexity,
			InputTokens:   u.In,
			OutputTokens:  u.Out,
			CacheRead:     u.CacheRead,
			CacheWrite:    u.CacheWrite,
			CostUSD:       cost,
			CacheSavedUSD: cacheSaved,
			LatencyMS:     rec.LatencyMS,
			Status:        status,
			Timestamp:     now.Format(time.RFC3339),
		})
		h.publishStats()
	}

	h.recordUsageEvent(active, req, stream, status, att, raw)
}

// recordUsageEvent persists one immutable per-attempt usage event. Called from
// logResult for attempts the client saw, and directly from discard sites
// (callUpstream's key rotation and transport errors, the retry/failover drops,
// cascade escalation, cooldown probes) for attempts it didn't. att == nil
// (defensive) or no DB configured means no event — never a panic.
func (h *Handler) recordUsageEvent(active *activeProvider, req AnthropicRequest, stream bool, status int, att *attemptInfo, raw rawUsage) {
	if h.db == nil || att == nil {
		return
	}
	ev := &storage.UsageEvent{
		CreatedAt:        time.Now(),
		Provider:         active.impl.Name(),
		ModelUsed:        h.upstreamModel(active, req.Model, req.nexusImages),
		ModelAsked:       req.Model,
		RequestID:        att.reqID,
		Attempt:          att.chainPos,
		Status:           status,
		Success:          status > 0 && status < 400,
		Stream:           stream,
		UsagePartial:     att.partial,
		DurationMS:       time.Since(att.started).Milliseconds(),
		RateLimited:      status == http.StatusTooManyRequests,
		RetryAfter:       att.quota.RetryAfter,
		RetryResetAt:     att.quota.RetryResetAt,
		QuotaDimension:   att.quota.Dimension,
		QuotaUtilization: att.quota.Utilization,
		QuotaResetAt:     att.quota.ResetAt,
		QuotaMeta:        att.quota.Meta,
		Error:            att.errText,
		Probe:            att.probe,
	}
	if att.keyIdx >= 0 {
		i := att.keyIdx
		ev.KeyIndex = &i
	}
	// Token fields are stored with cross-provider "fresh input" semantics:
	// in_tokens is always the portion of the prompt that was NOT served from
	// cache, matching what Anthropic reports natively. OpenAI-compatible
	// providers fold cached tokens into prompt_tokens, so the cached portion
	// is subtracted here (clamped at zero, presence preserved: a nil In or nil
	// CacheRead stays nil). The as-reported prompt_tokens stays reconstructible
	// as in + cache_read, and the window derivation can sum in + cache_read +
	// cache_write without double-counting for any provider.
	ev.In = raw.In
	ev.Out = raw.Out
	ev.CacheRead = raw.CacheRead
	ev.CacheWrite = raw.CacheWrite
	ev.Reasoning = raw.Reasoning
	if providers.IsOpenAICompatible(active.impl.Name()) && raw.In != nil && raw.CacheRead != nil {
		fresh := *raw.In - *raw.CacheRead
		if fresh < 0 {
			fresh = 0
		}
		ev.In = &fresh
	}
	if _, err := h.db.RecordUsageEvent(ev); err != nil {
		log.Warn().Err(err).Msg("Failed to record usage event")
	}
}

// publishStats computes today's aggregate stats and pushes them over SSE.
func (h *Handler) publishStats() {
	if h.db == nil || h.broker == nil {
		return
	}
	stats, err := h.db.GetStats("today")
	if err != nil {
		return
	}
	forecast, _ := h.db.GetCostForecast()
	h.broker.Publish("stats", map[string]interface{}{
		"total_requests":    stats.TotalRequests,
		"total_cost_usd":    stats.TotalCostUSD,
		"total_tokens":      stats.TotalInputTokens + stats.TotalOutputTokens,
		"forecast_usd":      forecast,
		"avg_latency_ms":    stats.AvgLatencyMS,
		"cache_saved_usd":   stats.CacheSavedUSD,
		"cache_read_tokens": stats.CacheReadTokens,
		"redacted_total":    stats.RedactedTotal,
	})
}

// serveCached writes a cached response and logs it as a (free, instant) cache hit.
func (h *Handler) serveCached(w http.ResponseWriter, e cacheEntry, start time.Time, user string) {
	if e.ctype != "" {
		w.Header().Set("Content-Type", e.ctype)
	}
	w.Header().Set("X-Nexus-Provider", "cache")
	w.Header().Set("X-Nexus-Cache", "HIT")
	w.WriteHeader(e.status)
	_, _ = w.Write(e.body)

	latency := time.Since(start)
	if h.db != nil {
		_, _ = h.db.LogRequest(&storage.Request{
			CreatedAt: time.Now(), Provider: "cache",
			ModelAsked: orStr(e.model, "cache"), ModelUsed: "cache", Complexity: "cached",
			InputTokens: e.in, OutputTokens: e.out, CostUSD: 0, LatencyMS: latency.Milliseconds(), Status: e.status,
			User: user,
		})
	}
	if h.broker != nil {
		h.broker.Publish("request", requestEvent{
			Provider: "cache", ModelAsked: orStr(e.model, "—"), ModelUsed: "cache", Complexity: "cached",
			InputTokens: e.in, OutputTokens: e.out, CostUSD: 0, LatencyMS: latency.Milliseconds(),
			Status: e.status, Timestamp: time.Now().Format(time.RFC3339),
		})
		h.publishStats()
	}
	log.Info().Int64("latency_ms", latency.Milliseconds()).Int("in", e.in).Int("out", e.out).Msg("Cache hit ⚡ (free)")
}

func orStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// writeError writes a JSON error response in Anthropic's error shape.
func (h *Handler) writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]string{
			"type":    "proxy_error",
			"message": message,
		},
	})
}

// ─── Header & parsing helpers ──────────────────────────────────────────────

// hopByHopHeaders should not be forwarded verbatim between client and provider.
// Content-Length and Content-Encoding are dropped because the body is re-read
// (and transparently decompressed) by the Go transport before we relay it.
var hopByHopHeaders = map[string]bool{
	"Connection":        true,
	"Proxy-Connection":  true,
	"Keep-Alive":        true,
	"Transfer-Encoding": true,
	"Te":                true,
	"Trailer":           true,
	"Upgrade":           true,
	"Content-Length":    true,
	"Content-Encoding":  true,
}

// copyResponseHeaders copies non-hop-by-hop headers from src into dst.
func copyResponseHeaders(dst, src http.Header) {
	for k, vals := range src {
		if hopByHopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}

// extractAPIKey pulls the API key from x-api-key or a Bearer Authorization header.
func extractAPIKey(h http.Header) string {
	if key := h.Get("x-api-key"); key != "" {
		return key
	}
	if auth := h.Get("Authorization"); auth != "" {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

// maxProviderAttempts is how many times the chain-walk loop in HandleMessages
// tries a single chain entry against non-429 retryable failures (500/502/
// 503/504/transport error) before treating it as failed for this request and
// advancing to the next chain entry. 429s are not counted here — they
// already rotate keys within callUpstream itself (P1) before ever reaching
// this retry loop.
const maxProviderAttempts = 3

// isRetryableStatus reports whether an upstream HTTP status should trigger
// failover to the next provider in the chain (rate-limit / transient errors).
func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	}
	return false
}

// ─── Budget tracking ───────────────────────────────────────────────────────

// budgetTracker enforces a soft daily spend cap. When exceeded, the handler
// restricts routing to free/local providers until the next day.
type budgetTracker struct {
	mu    sync.Mutex
	limit float64
	day   string
	spent float64
	// Alerts: when limit > 0 and a webhook is set, post once when crossing the
	// warn threshold and once when exceeding the budget (per day).
	webhook              string
	threshold            float64
	firedWarn, firedOver bool
	client               *http.Client
}

func newBudgetTracker(limit, spentToday float64, webhook string, threshold float64) *budgetTracker {
	if threshold <= 0 || threshold >= 1 {
		threshold = 0.8
	}
	return &budgetTracker{
		limit: limit, day: todayKey(), spent: spentToday,
		webhook: webhook, threshold: threshold,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func todayKey() string { return time.Now().Format("2006-01-02") }

// roll resets the running total when the day changes (caller holds b.mu).
func (b *budgetTracker) roll() {
	if d := todayKey(); d != b.day {
		b.day = d
		b.spent = 0
		b.firedWarn, b.firedOver = false, false
	}
}

func (b *budgetTracker) Add(cost float64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.roll()
	b.spent += cost
	var alert string
	if b.limit > 0 && b.webhook != "" {
		switch {
		case !b.firedOver && b.spent >= b.limit:
			b.firedOver = true
			alert = fmt.Sprintf("🔴 NEXUS: daily budget exceeded — $%.2f of $%.2f. Free/local models only for the rest of today.", b.spent, b.limit)
		case !b.firedWarn && b.spent >= b.limit*b.threshold:
			b.firedWarn = true
			alert = fmt.Sprintf("🟠 NEXUS: %.0f%% of your daily budget used — $%.2f of $%.2f.", b.threshold*100, b.spent, b.limit)
		}
	}
	b.mu.Unlock()
	if alert != "" {
		go b.postAlert(alert)
	}
}

// postAlert sends a one-line message to the configured webhook. The payload sets
// both "text" (Slack) and "content" (Discord) so a single URL works for either.
func (b *budgetTracker) postAlert(msg string) {
	payload, _ := json.Marshal(map[string]string{"text": msg, "content": msg})
	req, err := http.NewRequest("POST", b.webhook, bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		log.Warn().Err(err).Msg("Budget alert webhook failed")
		return
	}
	_ = resp.Body.Close()
}

func (b *budgetTracker) Over() bool {
	if b == nil || b.limit <= 0 {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.roll()
	return b.spent >= b.limit
}

// freeLocalOnly filters a route chain down to free/local providers.
func freeLocalOnly(chain []*router.Provider) []*router.Provider {
	var out []*router.Provider
	for _, p := range chain {
		if p.Tier == "free" || p.Tier == "local" {
			out = append(out, p)
		}
	}
	return out
}

// parseAnthropicUsage extracts token usage from a non-streaming Anthropic response.
func parseAnthropicUsage(body []byte) (in, out int) {
	var r struct {
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(body, &r)
	return r.Usage.InputTokens, r.Usage.OutputTokens
}

// ─── Types ─────────────────────────────────────────────────────────────────

// requestEvent is the payload pushed to the dashboard after each request.
type requestEvent struct {
	ID            int64   `json:"id"`
	Provider      string  `json:"provider"`
	ModelAsked    string  `json:"model_asked"`
	ModelUsed     string  `json:"model_used"`
	Complexity    string  `json:"complexity"`
	InputTokens   int     `json:"input_tokens"`
	OutputTokens  int     `json:"output_tokens"`
	CacheRead     int     `json:"cache_read,omitempty"`
	CacheWrite    int     `json:"cache_write,omitempty"`
	CostUSD       float64 `json:"cost_usd"`
	CacheSavedUSD float64 `json:"cache_saved_usd,omitempty"`
	LatencyMS     int64   `json:"latency_ms"`
	Status        int     `json:"status"`
	Timestamp     string  `json:"timestamp"`
}

// AnthropicRequest represents an incoming Claude Code request
type AnthropicRequest struct {
	Model         string        `json:"model"`
	Messages      []Message     `json:"messages"`
	MaxTokens     int           `json:"max_tokens"`
	Stream        bool          `json:"stream"`
	System        interface{}   `json:"system,omitempty"`
	Tools         []interface{} `json:"tools,omitempty"`
	nexusUser     string        // team attribution; derived per request, never serialized
	nexusRedacted int           // # secrets/PII the firewall masked; never serialized
	nexusImages   bool          // has ≥1 image content block; derived per request, never serialized
}

// deriveUser attributes a request to a team member: the X-Nexus-User header, or
// a "nexus-<name>" API key (so each dev just sets ANTHROPIC_API_KEY=nexus-alice).
func deriveUser(h http.Header) string {
	if u := h.Get("X-Nexus-User"); u != "" {
		return u
	}
	if key := extractAPIKey(h); strings.HasPrefix(key, "nexus-") && key != "nexus-local" {
		return strings.TrimPrefix(key, "nexus-")
	}
	return ""
}

// Message represents a single message in a conversation
type Message struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}

// ProviderConfig holds provider connection details
type ProviderConfig struct {
	Name    string
	BaseURL string
	APIKey  string
	Model   string // overridden model name (if any)
}
