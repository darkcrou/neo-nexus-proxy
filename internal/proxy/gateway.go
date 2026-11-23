package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/lynuxis2026-pixel/nexus-proxy/internal/providers"
	"github.com/lynuxis2026-pixel/nexus-proxy/internal/router"
)

// HandleChatCompletions is the OpenAI-compatible inbound endpoint
// (POST /v1/chat/completions). It lets ANY OpenAI-SDK client — Cursor, aider,
// Continue, Cline, Zed, LangChain, … — route through NEXUS, not just Claude Code.
func (h *Handler) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.writeOpenAIError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	defer r.Body.Close()

	user := deriveUser(r.Header) // team attribution

	// Privacy firewall: mask secrets/PII before anything downstream sees the body.
	var restoreMap map[string]string
	if h.firewall != nil {
		if red, m := h.firewall.redact(body); len(m) > 0 {
			body, restoreMap = red, m
		}
	}

	// Response cache: serve identical requests instantly (and free).
	if h.cache != nil {
		key := cacheKey("c", body)
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

	if restoreMap != nil {
		rw := newRestoringWriter(w, restoreMap)
		defer rw.flush()
		w = rw
	}

	var oreq OpenAIRequest
	if err := json.Unmarshal(body, &oreq); err != nil {
		log.Error().Err(err).Str("body", capForLog(body)).Msg("Failed to parse request body as JSON")
		h.writeOpenAIError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	// Raw map preserves every field (temperature, top_p, …) for high-fidelity
	// pass-through to OpenAI-compatible providers.
	var rawMap map[string]interface{}
	_ = json.Unmarshal(body, &rawMap)
	var rawMsgs struct {
		Messages []map[string]interface{} `json:"messages"`
	}
	_ = json.Unmarshal(body, &rawMsgs)

	hasTools := len(oreq.Tools) > 0
	complexity := router.ClassifyRequest(oreq.Model, rawMsgs.Messages, hasTools)
	areq := TransformOpenAIToAnthropic(oreq) // internal form for Anthropic providers + logging
	areq.nexusUser = user
	areq.nexusRedacted = len(restoreMap)
	// images drive the vision_model override and the model_used attribution
	// (same signal /v1/messages derives in HandleMessages)
	areq.nexusImages = messagesHaveImage(rawMsgs.Messages)

	log.Debug().Str("model", oreq.Model).Str("complexity", complexity.String()).
		Bool("stream", oreq.Stream).Bool("tools", hasTools).Msg("Incoming request (openai gateway)")

	if len(h.providers) == 0 {
		h.writeOpenAIError(w, http.StatusBadGateway, "no providers configured — add one with `nexus add <provider> <key>`")
		return
	}
	_ = h.catalog.syncQwenRevision(r.Context())
	if h.unknownModel(oreq.Model) {
		h.writeOpenAIError(w, http.StatusNotFound, "unknown model: "+oreq.Model)
		return
	}
	chain := h.stickyChain(h.router.RouteChain(oreq.Model, complexity), oreq.Model)
	if len(chain) == 0 {
		h.writeOpenAIError(w, http.StatusServiceUnavailable, "no providers available for model: "+oreq.Model)
		return
	}
	if h.budget.Over() {
		if cheap := freeLocalOnly(chain); len(cheap) > 0 {
			chain = cheap
		}
	}

	var credentialError error
	bearerRetried := false
	for i, cand := range chain {
		active := h.providers[cand.Name]
		if active == nil {
			continue
		}

		openAI := providers.IsOpenAICompatible(active.impl.Name())
		var resp *http.Response
		var att *attemptInfo
		var err error
		if openAI {
			// High-fidelity pass-through: forward the original OpenAI body (model swapped).
			resp, att, err = h.callWithKeys(r.Context(), active, areq, i+1, false, &bearerRetried, func(key string) (*http.Response, error) {
				return h.callOpenAIPassthrough(r.Context(), active, rawMap, areq, key)
			})
		} else {
			// Anthropic-format provider: convert OpenAI → Anthropic, then back to OpenAI.
			abody, _ := json.Marshal(areq)
			resp, att, err = h.callUpstream(r.Context(), active, areq, abody, r.Header, i+1, &bearerRetried)
		}
		if err != nil {
			if isQwenCredentialError(err) {
				credentialError = err
			}
			log.Warn().Str("provider", cand.Name).Err(err).Msg("Provider unreachable, trying next")
			continue
		}
		if (i < len(chain)-1 || credentialError != nil) && h.shouldFailOver(resp.StatusCode, oreq.Model) {
			h.recordUsageEvent(active, areq, oreq.Stream, resp.StatusCode, att, rawUsage{})
			resp.Body.Close()
			log.Warn().Str("provider", cand.Name).Int("status", resp.StatusCode).Msg("Retryable error, failing over")
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			h.setSticky(oreq.Model, cand.Name)
		}
		switch {
		case openAI && oreq.Stream && resp.StatusCode < 400:
			h.relayOpenAIPassthroughStream(w, r, active, areq, resp, startTime, complexity, att)
		case openAI:
			h.relayOpenAIPassthrough(w, active, areq, resp, startTime, complexity, att)
		default:
			h.relayAnthropicToOpenAI(w, active, areq, oreq, resp, startTime, complexity, att)
		}
		return
	}
	if credentialError != nil {
		h.writeOpenAIError(w, http.StatusServiceUnavailable, qwenCredentialMessage(credentialError))
		return
	}
	h.writeOpenAIError(w, http.StatusBadGateway, "all providers unreachable")
}

// stubModels is the list /v1/models served before the catalog existed: the three
// Claude ids Claude Code asks for. It is the last rung of the fallback ladder, so a
// client never sees an empty list even with no provider configured at all.
var stubModels = []providers.ListedModel{
	{ID: "claude-opus-4-5"},
	{ID: "claude-sonnet-4-6"},
	{ID: "claude-haiku-4-5"},
}

// modelListEntry is one entry of the OpenAI-shaped model list. There is
// deliberately no created field: NEXUS knows no honest creation time for a
// discovered id, and inventing one would be fabricated metadata (D-10). owned_by
// is "nexus" because the deduplicated entry names a capability the proxy offers,
// not the provider that happens to serve it.
type modelListEntry struct {
	ID              string    `json:"id"`
	Object          string    `json:"object"`
	OwnedBy         string    `json:"owned_by"`
	InputModalities *[]string `json:"input_modalities,omitempty"` // nil means unknown
}

// listedModels returns the snapshot, which already includes any configured or
// default seeds, or falls back to the stub list when the snapshot is empty.
func (h *Handler) listedModels() []providers.ListedModel {
	if h.catalog != nil {
		if models := h.catalog.snapshot(); len(models) > 0 {
			return models
		}
	}
	return slices.Clone(stubModels)
}

// HandleModels serves GET /v1/models (some OpenAI clients query it on startup).
//
// The answer is the cached catalog snapshot, including reported capabilities.
// Serving the list never triggers discovery.
func (h *Handler) HandleModels(w http.ResponseWriter, r *http.Request) {
	_ = h.catalog.syncQwenRevision(r.Context())
	models := h.listedModels()
	data := make([]modelListEntry, 0, len(models))
	for i := range models {
		model := &models[i]
		entry := modelListEntry{ID: model.ID, Object: "model", OwnedBy: "nexus"}
		if model.InputModalities != nil {
			entry.InputModalities = &model.InputModalities
		}
		data = append(data, entry)
	}
	writeJSONStatus(w, http.StatusOK, map[string]interface{}{"object": "list", "data": data})
}

// ─── upstream call (OpenAI pass-through) ────────────────────────────────────

func (h *Handler) callOpenAIPassthrough(ctx context.Context, active *activeProvider, rawMap map[string]interface{}, areq AnthropicRequest, key string) (*http.Response, error) {
	m := make(map[string]interface{}, len(rawMap)+2)
	for k, v := range rawMap {
		m[k] = v
	}
	m["model"] = h.upstreamModel(active, areq.Model, areq.nexusImages)
	m["stream"] = areq.Stream
	if areq.Stream {
		m["stream_options"] = map[string]interface{}{"include_usage": true}
	} else {
		delete(m, "stream_options")
	}
	payload, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errLocalPrep, err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", active.impl.ChatCompletionsURL(), bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errLocalPrep, err)
	}
	req.Header.Set("Content-Type", "application/json")
	h.authorize(active, req, payload, key)
	return h.doProviderRequest(active, req)
}

// ─── relays (OpenAI out) ────────────────────────────────────────────────────

func (h *Handler) relayOpenAIPassthrough(w http.ResponseWriter, active *activeProvider, areq AnthropicRequest, resp *http.Response, startTime time.Time, complexity router.Complexity, att *attemptInfo) {
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	copyResponseHeaders(w.Header(), resp.Header)
	w.Header().Set("X-Nexus-Provider", active.impl.Name())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)

	raw := openAIRawUsage(respBody)
	u := raw.openAITokens()
	h.logResult(active, areq, complexity, u, respBody, resp.StatusCode, time.Since(startTime), false, att, raw)
	log.Info().Str("provider", active.impl.Name()).Int("status", resp.StatusCode).
		Int("in", u.In).Int("out", u.Out).Int("cache_read", u.CacheRead).Str("complexity", complexity.String()).Msg("Request completed (gateway)")
}

func (h *Handler) relayOpenAIPassthroughStream(w http.ResponseWriter, r *http.Request, active *activeProvider, areq AnthropicRequest, resp *http.Response, startTime time.Time, complexity router.Complexity, att *attemptInfo) {
	defer resp.Body.Close()
	flusher, ok := w.(http.Flusher)
	if !ok {
		h.writeOpenAIError(w, http.StatusInternalServerError, "streaming not supported by server")
		return
	}
	copyResponseHeaders(w.Header(), resp.Header)
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "text/event-stream")
	}
	w.Header().Set("X-Nexus-Provider", active.impl.Name())
	w.WriteHeader(resp.StatusCode)
	flusher.Flush()

	var captured bytes.Buffer
	buf := make([]byte, 4096)
	// abort records the partial stream; the captured bytes are scraped for
	// whatever usage lines arrived before the break
	abort := func(abortText string) {
		raw := openAIRawUsage(captured.Bytes())
		h.logAbortedStream(active, areq, complexity, raw.openAITokens(), captured.Bytes(), resp.StatusCode, startTime, att, raw, abortText)
	}
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if captured.Len() < 1<<20 {
				captured.Write(buf[:n])
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				abort(abortClientGone)
				return
			}
			flusher.Flush()
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			log.Error().Err(readErr).Msg("Upstream stream read error (gateway)")
			abort(abortUpstreamErr)
			return
		}
		// Stop consuming upstream once the client is gone.
		select {
		case <-r.Context().Done():
			abort(abortClientGone)
			return
		default:
		}
	}
	raw := openAIRawUsage(captured.Bytes())
	u := raw.openAITokens()
	h.logResult(active, areq, complexity, u, captured.Bytes(), resp.StatusCode, time.Since(startTime), true, att, raw)
	log.Info().Str("provider", active.impl.Name()).Int("in", u.In).Int("out", u.Out).Int("cache_read", u.CacheRead).Bool("stream", true).Msg("Stream completed (gateway)")
}

// relayAnthropicToOpenAI converts an Anthropic provider response into OpenAI format.
func (h *Handler) relayAnthropicToOpenAI(w http.ResponseWriter, active *activeProvider, areq AnthropicRequest, oreq OpenAIRequest, resp *http.Response, startTime time.Time, complexity router.Complexity, att *attemptInfo) {
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	raw := anthropicRawUsage(respBody)
	if resp.StatusCode >= 400 {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Nexus-Provider", active.impl.Name())
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
		h.logResult(active, areq, complexity, tokenUsage{}, respBody, resp.StatusCode, time.Since(startTime), oreq.Stream, att, raw)
		return
	}
	var ar AnthropicResponse
	_ = json.Unmarshal(respBody, &ar)
	oaiResp := anthropicRespToOpenAIMap(ar, oreq.Model)
	u := tokenUsage{
		In:         ar.Usage.InputTokens,
		Out:        ar.Usage.OutputTokens,
		CacheRead:  ar.Usage.CacheReadInputTokens,
		CacheWrite: ar.Usage.CacheCreationInputTokens,
	}

	if oreq.Stream {
		writeOpenAISSE(w, active.impl.Name(), oaiResp)
	} else {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Nexus-Provider", active.impl.Name())
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(oaiResp)
	}
	h.logResult(active, areq, complexity, u, respBody, http.StatusOK, time.Since(startTime), oreq.Stream, att, raw)
	log.Info().Str("provider", active.impl.Name()).Int("in", u.In).Int("out", u.Out).Bool("stream", oreq.Stream).Msg("Request completed (gateway→anthropic)")
}

// writeOpenAISSE synthesizes an OpenAI streaming response from a full response map.
func writeOpenAISSE(w http.ResponseWriter, provider string, resp map[string]interface{}) {
	w.Header().Set("X-Nexus-Provider", provider)
	flusher, ok := w.(http.Flusher)
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	id, _ := resp["id"].(string)
	model, _ := resp["model"].(string)
	var text string
	finish := "stop"
	if choices, ok := resp["choices"].([]map[string]interface{}); ok && len(choices) > 0 {
		if msg, ok := choices[0]["message"].(map[string]interface{}); ok {
			text, _ = msg["content"].(string)
		}
		if fr, ok := choices[0]["finish_reason"].(string); ok && fr != "" {
			finish = fr
		}
	}
	send := func(payload map[string]interface{}) {
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	chunk := func(delta map[string]interface{}, finishReason interface{}) map[string]interface{} {
		return map[string]interface{}{
			"id": id, "object": "chat.completion.chunk", "model": model,
			"choices": []map[string]interface{}{{"index": 0, "delta": delta, "finish_reason": finishReason}},
		}
	}
	send(chunk(map[string]interface{}{"role": "assistant"}, nil))
	if text != "" {
		send(chunk(map[string]interface{}{"content": text}, nil))
	}
	send(chunk(map[string]interface{}{}, finish))
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (h *Handler) writeOpenAIError(w http.ResponseWriter, status int, message string) {
	writeJSONStatus(w, status, map[string]interface{}{
		"error": map[string]interface{}{"message": message, "type": "nexus_error"},
	})
}

func writeJSONStatus(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

// ─── transforms ─────────────────────────────────────────────────────────────

// TransformOpenAIToAnthropic converts an inbound OpenAI request to the internal
// Anthropic request shape (used for Anthropic providers + classification/logging).
func TransformOpenAIToAnthropic(o OpenAIRequest) AnthropicRequest {
	ar := AnthropicRequest{Model: o.Model, MaxTokens: o.MaxTokens, Stream: o.Stream}
	if ar.MaxTokens == 0 {
		ar.MaxTokens = 4096
	}
	var sys string
	for _, m := range o.Messages {
		if m.Role == "system" {
			sys += m.Content.Text()
			continue
		}
		// Anthropic().Content preserves the original array-of-text-blocks structure
		// when the client sent content as an array, instead of joining it into one
		// string — Anthropic-format providers accept the same block shape natively.
		ar.Messages = append(ar.Messages, Message{Role: m.Role, Content: m.Content.Anthropic()})
	}
	if sys != "" {
		ar.System = sys
	}
	for _, t := range o.Tools {
		ar.Tools = append(ar.Tools, map[string]interface{}{
			"name": t.Function.Name, "description": t.Function.Description, "input_schema": t.Function.Parameters,
		})
	}
	return ar
}

// anthropicRespToOpenAIMap converts an Anthropic response to an OpenAI response map.
func anthropicRespToOpenAIMap(a AnthropicResponse, model string) map[string]interface{} {
	msg := map[string]interface{}{"role": "assistant"}
	var content string
	var toolCalls []map[string]interface{}
	for _, b := range a.Content {
		switch b.Type {
		case "text":
			content += b.Text
		case "tool_use":
			args, _ := json.Marshal(b.Input)
			toolCalls = append(toolCalls, map[string]interface{}{
				"id": b.ID, "type": "function",
				"function": map[string]interface{}{"name": b.Name, "arguments": string(args)},
			})
		}
	}
	msg["content"] = content
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	id := a.ID
	if id == "" {
		id = "chatcmpl-nexus"
	}
	return map[string]interface{}{
		"id": "chatcmpl-" + id, "object": "chat.completion", "model": model,
		"choices": []map[string]interface{}{{
			"index": 0, "message": msg, "finish_reason": anthropicStopToOpenAI(a.StopReason),
		}},
		"usage": map[string]interface{}{
			"prompt_tokens": a.Usage.InputTokens, "completion_tokens": a.Usage.OutputTokens,
			"total_tokens": a.Usage.InputTokens + a.Usage.OutputTokens,
		},
	}
}

func anthropicStopToOpenAI(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

var (
	reOAIPrompt     = regexp.MustCompile(`"prompt_tokens":\s*(\d+)`)
	reOAICompletion = regexp.MustCompile(`"completion_tokens":\s*(\d+)`)
)

func parseOpenAIUsage(data []byte) (in, out int) {
	if m := reOAIPrompt.FindSubmatch(data); m != nil {
		in, _ = strconv.Atoi(string(m[1]))
	}
	if all := reOAICompletion.FindAllSubmatch(data, -1); len(all) > 0 {
		out, _ = strconv.Atoi(string(all[len(all)-1][1]))
	}
	return in, out
}
