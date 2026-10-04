package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
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

	log.Debug().Str("model", oreq.Model).Str("complexity", complexity.String()).
		Bool("stream", oreq.Stream).Bool("tools", hasTools).Msg("Incoming request (openai gateway)")

	chain := h.router.RouteChain(oreq.Model, complexity)
	if len(h.providers) == 0 || len(chain) == 0 {
		h.writeOpenAIError(w, http.StatusBadGateway, "no providers configured — add one with `nexus add <provider> <key>`")
		return
	}
	if h.budget.Over() {
		if cheap := freeLocalOnly(chain); len(cheap) > 0 {
			chain = cheap
		}
	}

	for i, cand := range chain {
		active := h.providers[cand.Name]
		if active == nil {
			continue
		}

		if providers.IsOpenAICompatible(active.impl.Name()) {
			// High-fidelity pass-through: forward the original OpenAI body (model swapped).
			resp, att, err := h.callOpenAIPassthrough(active, rawMap, oreq.Stream, i+1)
			if err != nil {
				log.Warn().Str("provider", cand.Name).Err(err).Msg("Provider unreachable, trying next")
				continue
			}
			if isRetryableStatus(resp.StatusCode) && i < len(chain)-1 {
				h.recordUsageEvent(active, areq, oreq.Stream, resp.StatusCode, att, rawUsage{})
				resp.Body.Close()
				log.Warn().Str("provider", cand.Name).Int("status", resp.StatusCode).Msg("Retryable error, failing over")
				continue
			}
			if oreq.Stream {
				h.relayOpenAIPassthroughStream(w, r, active, areq, resp, startTime, complexity, att)
			} else {
				h.relayOpenAIPassthrough(w, active, areq, resp, startTime, complexity, att)
			}
			return
		}

		// Anthropic-format provider: convert OpenAI → Anthropic, then back to OpenAI.
		abody, _ := json.Marshal(areq)
		resp, att, err := h.callUpstream(active, areq, abody, r.Header, i+1)
		if err != nil {
			log.Warn().Str("provider", cand.Name).Err(err).Msg("Provider unreachable, trying next")
			continue
		}
		if isRetryableStatus(resp.StatusCode) && i < len(chain)-1 {
			h.recordUsageEvent(active, areq, oreq.Stream, resp.StatusCode, att, rawUsage{})
			resp.Body.Close()
			continue
		}
		h.relayAnthropicToOpenAI(w, active, areq, oreq, resp, startTime, complexity, att)
		return
	}
	h.writeOpenAIError(w, http.StatusBadGateway, "all providers unreachable")
}

// HandleModels serves GET /v1/models (some OpenAI clients query it on startup).
func (h *Handler) HandleModels(w http.ResponseWriter, r *http.Request) {
	models := []map[string]interface{}{}
	seen := map[string]bool{}
	for _, id := range []string{"claude-opus-4-5", "claude-sonnet-4-6", "claude-haiku-4-5"} {
		models = append(models, map[string]interface{}{"id": id, "object": "model", "owned_by": "nexus"})
		seen[id] = true
	}
	writeJSONStatus(w, http.StatusOK, map[string]interface{}{"object": "list", "data": models})
}

// ─── upstream call (OpenAI pass-through) ────────────────────────────────────

func (h *Handler) callOpenAIPassthrough(active *activeProvider, rawMap map[string]interface{}, stream bool, chainPos int) (*http.Response, *attemptInfo, error) {
	key, idx, ok := active.pickKey()
	if !ok {
		return nil, nil, errProviderExhausted
	}
	start := time.Now()
	m := make(map[string]interface{}, len(rawMap)+2)
	for k, v := range rawMap {
		m[k] = v
	}
	inModel, _ := rawMap["model"].(string)
	m["model"] = h.mappedModel(active, inModel)
	m["stream"] = stream
	if stream {
		m["stream_options"] = map[string]interface{}{"include_usage": true}
	} else {
		delete(m, "stream_options")
	}
	payload, err := json.Marshal(m)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", errLocalPrep, err)
	}
	req, err := http.NewRequest("POST", active.impl.ChatCompletionsURL(), bytes.NewReader(payload))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", errLocalPrep, err)
	}
	req.Header.Set("Content-Type", "application/json")
	h.authorize(active, req, payload, key)
	resp, err := h.httpClient.Do(req)
	if err != nil {
		// inModel is in scope; keeping model attribution on the event makes
		// discard-site analysis consistent with every other recording site
		// (recordUsageEvent derives model_used from mappedModel itself)
		h.recordUsageEvent(active, AnthropicRequest{Model: inModel}, stream, 0, &attemptInfo{
			keyIdx: idx, chainPos: chainPos, started: start,
			errText: "transport: " + err.Error(),
		}, rawUsage{})
		return nil, nil, err
	}
	return resp, &attemptInfo{
		keyIdx: idx, chainPos: chainPos, started: start,
		quota: captureQuota(resp.Header), reqID: extractRequestID(resp.Header),
	}, nil
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
