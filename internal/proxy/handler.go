// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NEXUS contributors

package proxy

import (
	"bytes"
	"encoding/json"
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

// activeProvider bundles a provider implementation with its API key pool. NEXUS
// round-robins across keys and puts a key on a short cooldown when it returns a
// 429, so a pool of free-tier keys behaves like one larger free quota.
type activeProvider struct {
	impl   providers.Provider
	apiKey string // primary key (keys[0]) — used for health checks and single-key paths

	mu   sync.Mutex
	keys []string
	rr   uint32
	cool []time.Time // per-key "cooling until" timestamps
}

// pickKey returns the next non-cooling key (round-robin) and its index. With no
// key pool it falls back to apiKey (index -1). If every key is cooling it
// returns the next in rotation anyway (best effort).
func (a *activeProvider) pickKey() (string, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := len(a.keys)
	if n == 0 {
		return a.apiKey, -1
	}
	now := time.Now()
	for off := 0; off < n; off++ {
		a.rr = (a.rr + 1) % uint32(n)
		if a.cool[a.rr].Before(now) {
			return a.keys[a.rr], int(a.rr)
		}
	}
	return a.keys[a.rr], int(a.rr)
}

// penalize puts a key on cooldown after a rate-limit response.
func (a *activeProvider) penalize(idx int, d time.Duration) {
	if idx < 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if idx < len(a.cool) {
		a.cool[idx] = time.Now().Add(d)
	}
}

// Handler handles incoming Claude Code requests.
type Handler struct {
	httpClient *http.Client
	router     *router.Router
	providers  map[string]*activeProvider
	db         *storage.DB
	broker     EventPublisher // may be nil
	budget     *budgetTracker
	cache      *responseCache // may be nil (disabled)
	cascade    bool           // cheap-first cascade with verification
	firewall   *redactor      // privacy firewall (nil = off)
	inspect    bool           // capture full prompt/response for the inspector
	rules      []config.Rule  // declarative routing overrides
	maxReqUSD  float64        // guardrail: downgrade a single request above this
	stopHealth chan struct{}
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
			Name:        pc.Name,
			Type:        pc.Type,
			APIKey:      key,
			BaseURL:     pc.BaseURL,
			Models:      pc.Models,
			Tier:        pc.Tier,
			ModelMap:    pc.ModelMap,
			InputPer1M:  pc.InputPer1M,
			OutputPer1M: pc.OutputPer1M,
			OffPeakInputPer1M:  pc.OffPeakInputPer1M,
			OffPeakOutputPer1M: pc.OffPeakOutputPer1M,
			OffPeakStartUTC:    pc.OffPeakStartUTC,
			OffPeakEndUTC:      pc.OffPeakEndUTC,
			Region:      pc.Region,
			Project:     pc.Project,
			APIVersion:  pc.APIVersion,
		})
		if err != nil {
			log.Warn().Str("provider", pc.Name).Err(err).Msg("Skipping provider")
			continue
		}
		active[impl.Name()] = &activeProvider{impl: impl, apiKey: key, keys: keys, cool: make([]time.Time, len(keys))}
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
		go h.healthLoop() // periodic background health checks
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

// healthLoop periodically health-checks every provider and updates the router,
// so unhealthy providers are skipped (and recover automatically).
func (h *Handler) healthLoop() {
	stop := h.stopHealth
	check := func() {
		var wg sync.WaitGroup
		for name, ap := range h.providers {
			wg.Add(1)
			go func(name string, ap *activeProvider) {
				defer wg.Done()
				h.router.SetHealthy(name, ap.impl.HealthCheck() == nil)
			}(name, ap)
		}
		wg.Wait()
	}
	check() // initial pass shortly after startup
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			check()
		}
	}
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
			if text, ht, ok := promptText(body); ok {
				hasTools = ht
				if !ht {
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

	// Routing rules + cost guardrail need the prompt text.
	var ptext string
	if len(h.rules) > 0 || h.maxReqUSD > 0 {
		ptext, _, _ = promptText(body)
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
		resp, err := h.callUpstream(active, req, body, r.Header)
		if err != nil {
			log.Warn().Str("provider", cand.Name).Err(err).Msg("Provider unreachable, trying next")
			continue
		}
		if isRetryableStatus(resp.StatusCode) && i < len(chain)-1 {
			h.router.RecordOutcome(cand.Name, complexity, false)
			resp.Body.Close()
			log.Warn().Str("provider", cand.Name).Int("status", resp.StatusCode).Msg("Retryable error, failing over to next provider")
			continue
		}
		h.router.RecordOutcome(cand.Name, complexity, resp.StatusCode < 400)
		switch {
		case providers.IsOpenAICompatible(active.impl.Name()) && req.Stream:
			h.relayOpenAIStream(w, active, req, resp, startTime, complexity)
		case providers.IsOpenAICompatible(active.impl.Name()):
			h.relayOpenAI(w, active, req, resp, startTime, complexity)
		default:
			// Anthropic-format. Bedrock/Vertex return a full body (buffered);
			// native Anthropic streams through.
			if _, custom := active.impl.(providers.AnthropicNative); custom {
				h.relayAnthropicBuffered(w, active, req, resp, startTime, complexity)
			} else if req.Stream {
				h.relayAnthropicStream(w, r, active, req, resp, startTime, complexity)
			} else {
				h.relayAnthropicSync(w, active, req, resp, startTime, complexity)
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

// callUpstream issues the upstream HTTP request, rotating across the provider's
// API key pool: on a 429 it cools the current key and retries with the next one,
// so the handler only fails over to a different provider once every key for this
// provider is rate-limited. It returns a transport error only — provider HTTP
// errors come back in *http.Response.
func (h *Handler) callUpstream(active *activeProvider, req AnthropicRequest, body []byte, origHeaders http.Header) (*http.Response, error) {
	attempts := len(active.keys)
	if attempts < 1 {
		attempts = 1
	}
	var resp *http.Response
	var err error
	for i := 0; i < attempts; i++ {
		key, idx := active.pickKey()
		resp, err = h.callUpstreamOnce(active, req, body, origHeaders, key)
		if err != nil {
			return resp, err
		}
		if resp.StatusCode == http.StatusTooManyRequests && i < attempts-1 {
			active.penalize(idx, 60*time.Second)
			resp.Body.Close()
			log.Warn().Str("provider", active.impl.Name()).Msg("key rate-limited (429), rotating to next key")
			continue
		}
		return resp, err
	}
	return resp, err
}

// callUpstreamOnce performs a single upstream request with a specific API key.
func (h *Handler) callUpstreamOnce(active *activeProvider, req AnthropicRequest, body []byte, origHeaders http.Header, key string) (*http.Response, error) {
	if providers.IsOpenAICompatible(active.impl.Name()) {
		oaiReq, err := TransformToOpenAI(req, active.impl.MapModel(req.Model))
		if err != nil {
			return nil, fmt.Errorf("request transform failed: %w", err)
		}
		oaiReq.Stream = req.Stream // stream upstream when the client streams
		if req.Stream {
			oaiReq.StreamOptions = &OpenAIStreamOptions{IncludeUsage: true}
		}
		payload, err := json.Marshal(oaiReq)
		if err != nil {
			return nil, err
		}
		httpReq, err := http.NewRequest("POST", active.impl.ChatCompletionsURL(), bytes.NewReader(payload))
		if err != nil {
			return nil, err
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
		return nil, err
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
	resp, err := h.callUpstream(active, req, body, r.Header)
	if err != nil {
		log.Error().Err(err).Msg("Provider request failed")
		h.writeError(w, http.StatusBadGateway, fmt.Sprintf("provider error: %v", err))
		return
	}
	if req.Stream {
		h.relayAnthropicStream(w, r, active, req, resp, startTime, complexity)
	} else {
		h.relayAnthropicSync(w, active, req, resp, startTime, complexity)
	}
}

// relayAnthropicSync relays a non-streaming native-Anthropic response.
func (h *Handler) relayAnthropicSync(w http.ResponseWriter, active *activeProvider, req AnthropicRequest, resp *http.Response, startTime time.Time, complexity router.Complexity) {
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

	u := anthropicUsageFull(respBody)
	h.logResult(active, req, complexity, u, respBody, resp.StatusCode, time.Since(startTime), false)
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
func (h *Handler) relayAnthropicBuffered(w http.ResponseWriter, active *activeProvider, req AnthropicRequest, resp *http.Response, startTime time.Time, complexity router.Complexity) {
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "failed to read provider response")
		return
	}

	if resp.StatusCode >= 400 {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Nexus-Provider", active.impl.Name())
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
		h.logResult(active, req, complexity, tokenUsage{}, respBody, resp.StatusCode, time.Since(startTime), req.Stream)
		log.Warn().Str("provider", active.impl.Name()).Int("status", resp.StatusCode).Msg("Provider returned error")
		return
	}

	u := anthropicUsageFull(respBody)
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

	h.logResult(active, req, complexity, u, respBody, http.StatusOK, time.Since(startTime), req.Stream)
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

// logResult records a completed request to storage and pushes live events.
// respBody is the upstream response (used only for --inspect capture; may be nil).
func (h *Handler) logResult(active *activeProvider, req AnthropicRequest, complexity router.Complexity, u tokenUsage, respBody []byte, status int, latency time.Duration, stream bool) {
	now := time.Now()
	pricing := active.impl.Pricing()
	cost := pricing.CalculateCostFullAt(u.In, u.Out, u.CacheRead, u.CacheWrite, now) // off-peak-aware
	cacheSaved := pricing.CacheReadSavings(u.CacheRead)
	h.budget.Add(cost)
	rec := &storage.Request{
		CreatedAt:        now,
		ModelAsked:       req.Model,
		ModelUsed:        active.impl.MapModel(req.Model),
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
	if h.inspect { // opt-in: capture full prompt + response for the inspector
		if pj, err := json.Marshal(req); err == nil {
			rec.Prompt = capText(string(pj))
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
			ID:           id,
			Provider:     rec.Provider,
			ModelAsked:   rec.ModelAsked,
			ModelUsed:    rec.ModelUsed,
			Complexity:   rec.Complexity,
			InputTokens:  u.In,
			OutputTokens: u.Out,
			CacheRead:    u.CacheRead,
			CacheWrite:   u.CacheWrite,
			CostUSD:      cost,
			CacheSavedUSD: cacheSaved,
			LatencyMS:    rec.LatencyMS,
			Status:       status,
			Timestamp:    now.Format(time.RFC3339),
		})
		h.publishStats()
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
		"total_requests":  stats.TotalRequests,
		"total_cost_usd":  stats.TotalCostUSD,
		"total_tokens":    stats.TotalInputTokens + stats.TotalOutputTokens,
		"forecast_usd":    forecast,
		"avg_latency_ms":  stats.AvgLatencyMS,
		"cache_saved_usd": stats.CacheSavedUSD,
		"cache_read_tokens": stats.CacheReadTokens,
		"redacted_total":  stats.RedactedTotal,
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
	Model     string        `json:"model"`
	Messages  []Message     `json:"messages"`
	MaxTokens int           `json:"max_tokens"`
	Stream    bool          `json:"stream"`
	System    interface{}   `json:"system,omitempty"`
	Tools         []interface{} `json:"tools,omitempty"`
	nexusUser     string        // team attribution; derived per request, never serialized
	nexusRedacted int           // # secrets/PII the firewall masked; never serialized
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
