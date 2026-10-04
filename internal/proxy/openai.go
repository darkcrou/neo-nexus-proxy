package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/lynuxis2026-pixel/nexus-proxy/internal/router"
)

// relayOpenAI handles a response from an OpenAI-compatible provider: it converts
// the OpenAI response back to Anthropic format. Because we always call upstream
// non-streaming, when the client asked for a stream we synthesize the Anthropic
// SSE event sequence from the complete response.
func (h *Handler) relayOpenAI(w http.ResponseWriter, active *activeProvider, req AnthropicRequest, resp *http.Response, startTime time.Time, complexity router.Complexity, att *attemptInfo) {
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "failed to read provider response")
		return
	}

	raw := openAIRawUsage(respBody)

	// Relay provider-side errors (bad key, rate limit, …) as-is.
	if resp.StatusCode >= 400 {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Nexus-Provider", active.impl.Name())
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
		h.logResult(active, req, complexity, tokenUsage{}, respBody, resp.StatusCode, time.Since(startTime), req.Stream, att, raw)
		log.Warn().Str("provider", active.impl.Name()).Int("status", resp.StatusCode).Msg("Provider returned error")
		return
	}

	var oaiResp OpenAIResponse
	if err := json.Unmarshal(respBody, &oaiResp); err != nil || len(oaiResp.Choices) == 0 {
		h.writeError(w, http.StatusBadGateway, "invalid response from provider")
		return
	}

	anthResp := TransformFromOpenAI(oaiResp, req.Model)
	u := raw.openAITokens() // captures DeepSeek/OpenAI prompt-cache tokens

	if req.Stream {
		writeAnthropicSSE(w, active.impl.Name(), anthResp)
	} else {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Nexus-Provider", active.impl.Name())
		w.Header().Set("X-Nexus-Latency", fmt.Sprintf("%dms", time.Since(startTime).Milliseconds()))
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(anthResp)
	}

	h.logResult(active, req, complexity, u, respBody, http.StatusOK, time.Since(startTime), req.Stream, att, raw)
	log.Info().
		Str("provider", active.impl.Name()).
		Str("model_used", h.mappedModel(active, req.Model)).
		Int("in", u.In).
		Int("out", u.Out).
		Int("cache_read", u.CacheRead).
		Int64("latency_ms", time.Since(startTime).Milliseconds()).
		Str("complexity", complexity.String()).
		Bool("stream", req.Stream).
		Msg("Request completed (openai)")
}

// writeAnthropicSSE synthesizes the Anthropic streaming event sequence from a
// complete AnthropicResponse, so OpenAI-compatible providers can be streamed to
// Claude Code transparently (as a single burst of well-formed SSE events).
func writeAnthropicSSE(w http.ResponseWriter, provider string, resp AnthropicResponse) {
	w.Header().Set("X-Nexus-Provider", provider)

	flusher, ok := w.(http.Flusher)
	if !ok {
		// No flusher available: fall back to a single JSON body.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	send := func(event string, payload map[string]interface{}) {
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		flusher.Flush()
	}

	send("message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            resp.ID,
			"type":          "message",
			"role":          "assistant",
			"model":         resp.Model,
			"content":       []interface{}{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]interface{}{"input_tokens": resp.Usage.InputTokens, "output_tokens": 0},
		},
	})

	index := 0
	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			send("content_block_start", map[string]interface{}{
				"type": "content_block_start", "index": index,
				"content_block": map[string]interface{}{"type": "text", "text": ""},
			})
			send("content_block_delta", map[string]interface{}{
				"type": "content_block_delta", "index": index,
				"delta": map[string]interface{}{"type": "text_delta", "text": block.Text},
			})
			send("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": index})
			index++
		case "tool_use":
			inputJSON, _ := json.Marshal(block.Input)
			send("content_block_start", map[string]interface{}{
				"type": "content_block_start", "index": index,
				"content_block": map[string]interface{}{"type": "tool_use", "id": block.ID, "name": block.Name, "input": map[string]interface{}{}},
			})
			send("content_block_delta", map[string]interface{}{
				"type": "content_block_delta", "index": index,
				"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": string(inputJSON)},
			})
			send("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": index})
			index++
		}
	}

	send("message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": resp.StopReason, "stop_sequence": nil},
		"usage": map[string]interface{}{"output_tokens": resp.Usage.OutputTokens},
	})
	send("message_stop", map[string]interface{}{"type": "message_stop"})
}

// oaiStreamChunk is one OpenAI streaming SSE delta.
type oaiStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens         int `json:"prompt_tokens"`
		CompletionTokens     int `json:"completion_tokens"`
		PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"` // DeepSeek
		PromptTokensDetails  struct {
			CachedTokens int `json:"cached_tokens"` // OpenAI
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails struct {
			ReasoningTokens *int `json:"reasoning_tokens"` // GLM/OpenAI reasoning — nil = not reported
		} `json:"completion_tokens_details"`
	} `json:"usage"`
}

// relayOpenAIStream converts a live OpenAI streaming response into the Anthropic
// SSE event sequence, forwarding tokens to Claude Code as they arrive. It stops
// consuming upstream as soon as the client goes away (write error or cancelled
// request context) or the upstream connection breaks, recording partial usage
// in either case.
func (h *Handler) relayOpenAIStream(w http.ResponseWriter, r *http.Request, active *activeProvider, req AnthropicRequest, resp *http.Response, startTime time.Time, complexity router.Complexity, att *attemptInfo) {
	defer resp.Body.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.writeError(w, http.StatusInternalServerError, "streaming not supported by server")
		return
	}

	// Relay an upstream error as a plain JSON body (no SSE).
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Nexus-Provider", active.impl.Name())
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
		h.logResult(active, req, complexity, tokenUsage{}, body, resp.StatusCode, time.Since(startTime), true, att, rawUsage{})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Nexus-Provider", active.impl.Name())
	w.WriteHeader(http.StatusOK)

	type toolAcc struct{ id, name, args string }
	tools := map[int]*toolAcc{}
	var toolOrder []int
	textOpen := false
	finish := "stop"
	inTok, outTok, cachedTok := 0, 0, 0
	reasonTok := 0
	// presence flags: a usage chunk seen means in/out were reported; cache and
	// reasoning are only claimed when the provider actually reported them
	sawUsage, sawReasoning := false, false

	send := func(event string, payload map[string]interface{}) error {
		b, _ := json.Marshal(payload)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	// abort records the partial stream on both records with whatever tokens
	// the upstream reported so far, so a disconnect stops consuming upstream
	// quota without losing the consumption that already happened
	abort := func(abortText string) {
		var raw rawUsage
		if sawUsage {
			raw.In, raw.Out = &inTok, &outTok
			if cachedTok > 0 {
				raw.CacheRead = &cachedTok
			}
			if sawReasoning {
				raw.Reasoning = &reasonTok
			}
		}
		h.logAbortedStream(active, req, complexity, tokenUsage{In: inTok - cachedTok, Out: outTok, CacheRead: cachedTok}, nil, resp.StatusCode, startTime, att, raw, abortText)
	}

	if err := send("message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id": "msg_" + active.impl.Name(), "type": "message", "role": "assistant",
			"model": req.Model, "content": []interface{}{},
			"stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]interface{}{"input_tokens": 0, "output_tokens": 0},
		},
	}); err != nil {
		abort(abortClientGone)
		return
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk oaiStreamChunk
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if chunk.Usage != nil {
			sawUsage = true
			inTok = chunk.Usage.PromptTokens
			outTok = chunk.Usage.CompletionTokens
			cachedTok = chunk.Usage.PromptCacheHitTokens
			if d := chunk.Usage.PromptTokensDetails.CachedTokens; d > cachedTok {
				cachedTok = d
			}
			if rt := chunk.Usage.CompletionTokensDetails.ReasoningTokens; rt != nil {
				reasonTok = *rt
				sawReasoning = true
			}
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		ch := chunk.Choices[0]
		if ch.Delta.Content != "" {
			if !textOpen {
				if err := send("content_block_start", map[string]interface{}{"type": "content_block_start", "index": 0, "content_block": map[string]interface{}{"type": "text", "text": ""}}); err != nil {
					abort(abortClientGone)
					return
				}
				textOpen = true
			}
			if err := send("content_block_delta", map[string]interface{}{"type": "content_block_delta", "index": 0, "delta": map[string]interface{}{"type": "text_delta", "text": ch.Delta.Content}}); err != nil {
				abort(abortClientGone)
				return
			}
		}
		for _, tc := range ch.Delta.ToolCalls {
			acc, exists := tools[tc.Index]
			if !exists {
				acc = &toolAcc{}
				tools[tc.Index] = acc
				toolOrder = append(toolOrder, tc.Index)
			}
			if tc.ID != "" {
				acc.id = tc.ID
			}
			if tc.Function.Name != "" {
				acc.name = tc.Function.Name
			}
			acc.args += tc.Function.Arguments
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			finish = *ch.FinishReason
		}

		// Stop consuming upstream once the client is gone.
		select {
		case <-r.Context().Done():
			abort(abortClientGone)
			return
		default:
		}
	}
	if err := scanner.Err(); err != nil {
		log.Error().Err(err).Msg("Upstream stream read error")
		abort(abortUpstreamErr)
		return
	}

	if textOpen {
		send("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": 0})
	}
	idx := 0
	if textOpen {
		idx = 1
	}
	for _, ti := range toolOrder {
		acc := tools[ti]
		send("content_block_start", map[string]interface{}{"type": "content_block_start", "index": idx, "content_block": map[string]interface{}{"type": "tool_use", "id": acc.id, "name": acc.name, "input": map[string]interface{}{}}})
		send("content_block_delta", map[string]interface{}{"type": "content_block_delta", "index": idx, "delta": map[string]interface{}{"type": "input_json_delta", "partial_json": acc.args}})
		send("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": idx})
		idx++
	}

	send("message_delta", map[string]interface{}{"type": "message_delta", "delta": map[string]interface{}{"stop_reason": mapStopReason(finish), "stop_sequence": nil}, "usage": map[string]interface{}{"output_tokens": outTok}})
	send("message_stop", map[string]interface{}{"type": "message_stop"})

	if cachedTok > inTok {
		cachedTok = inTok
	}
	// raw presence mirrors what the stream actually reported: no usage chunk
	// at all → all nil (never zeros); cache read only when a cache field was
	// non-zero; reasoning only when a reasoning field was present
	var raw rawUsage
	if sawUsage {
		raw.In, raw.Out = &inTok, &outTok
		if cachedTok > 0 {
			raw.CacheRead = &cachedTok
		}
		if sawReasoning {
			raw.Reasoning = &reasonTok
		}
	}
	u := tokenUsage{In: inTok - cachedTok, Out: outTok, CacheRead: cachedTok}
	h.logResult(active, req, complexity, u, nil, http.StatusOK, time.Since(startTime), true, att, raw)
	log.Info().
		Str("provider", active.impl.Name()).
		Int("in", u.In).Int("out", u.Out).Int("cache_read", u.CacheRead).
		Int64("latency_ms", time.Since(startTime).Milliseconds()).
		Bool("stream", true).
		Msg("Stream completed (openai live)")
}
