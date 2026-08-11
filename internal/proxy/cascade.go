package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/lynuxis2026-pixel/nexus-proxy/internal/providers"
	"github.com/lynuxis2026-pixel/nexus-proxy/internal/router"
)

// serveCascade tries providers cheapest-first, buffering and verifying each
// response, and escalating to the next (pricier) provider only when the cheap
// model's output fails a structural check. The last candidate is always served
// (best effort). Returns true if it handled the request.
//
// Upstream calls are forced non-streaming so the response can be buffered and
// verified; if the client asked to stream, the accepted response is re-streamed
// to it from the buffer by the standard relays.
func (h *Handler) serveCascade(w http.ResponseWriter, r *http.Request, req AnthropicRequest, body []byte, startTime time.Time, complexity router.Complexity, chain []*router.Provider) bool {
	ureq := req
	ureq.Stream = false
	noStreamBody := setStreamFalse(body)

	for i, cand := range chain {
		active := h.providers[cand.Name]
		if active == nil {
			continue
		}
		last := i == len(chain)-1

		resp, err := h.callUpstream(active, ureq, noStreamBody, r.Header)
		if err != nil {
			log.Warn().Str("provider", cand.Name).Err(err).Msg("cascade: provider unreachable, escalating")
			continue
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			h.router.RecordOutcome(active.impl.Name(), complexity, false)
			if last {
				h.relayBuffered(w, active, req, raw, resp, startTime, complexity)
				return true
			}
			log.Warn().Str("provider", cand.Name).Int("status", resp.StatusCode).Msg("cascade: upstream error, escalating")
			continue
		}

		if !last && !verifyResponse(active.impl.Name(), raw) {
			h.router.RecordOutcome(active.impl.Name(), complexity, false)
			log.Info().Str("provider", cand.Name).Msg("cascade: weak/invalid output, escalating to a stronger model")
			continue
		}
		h.router.RecordOutcome(active.impl.Name(), complexity, true)
		if !last {
			log.Info().Str("provider", cand.Name).Str("complexity", complexity.String()).Msg("cascade: cheap model accepted ✓")
		}
		h.relayBuffered(w, active, req, raw, resp, startTime, complexity)
		return true
	}
	return false
}

// relayBuffered serves a fully-buffered upstream response, reusing the standard
// relays (which synthesize streaming to the client when req.Stream is set).
func (h *Handler) relayBuffered(w http.ResponseWriter, active *activeProvider, req AnthropicRequest, raw []byte, orig *http.Response, startTime time.Time, complexity router.Complexity) {
	hdr := orig.Header
	if hdr == nil {
		hdr = http.Header{}
	}
	buffered := &http.Response{
		StatusCode: orig.StatusCode,
		Header:     hdr,
		Body:       io.NopCloser(bytes.NewReader(raw)),
	}
	if providers.IsOpenAICompatible(active.impl.Name()) {
		h.relayOpenAI(w, active, req, buffered, startTime, complexity)
		return
	}
	h.relayAnthropicBuffered(w, active, req, buffered, startTime, complexity)
}

// verifyResponse is a cheap, deterministic structural check: a usable answer has
// non-empty text, or at least one tool call whose arguments parse as JSON. It
// catches the common weak-model failure modes (empty output, malformed tool
// calls) without spending money on a separate verifier call.
func verifyResponse(name string, raw []byte) bool {
	if providers.IsOpenAICompatible(name) {
		var r OpenAIResponse
		if json.Unmarshal(raw, &r) != nil || len(r.Choices) == 0 {
			return false
		}
		msg := r.Choices[0].Message
		if strings.TrimSpace(msg.Content.Text()) != "" {
			return true
		}
		for _, tc := range msg.ToolCalls {
			var v interface{}
			if json.Unmarshal([]byte(tc.Function.Arguments), &v) == nil {
				return true
			}
		}
		return false
	}
	var r AnthropicResponse
	if json.Unmarshal(raw, &r) != nil {
		return false
	}
	for _, b := range r.Content {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			return true
		}
		if b.Type == "tool_use" && b.Name != "" {
			return true
		}
	}
	return false
}

// setStreamFalse returns body with "stream":false so cascade attempts can be
// buffered and verified before anything is sent to the client.
func setStreamFalse(body []byte) []byte {
	var m map[string]interface{}
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	m["stream"] = false
	if b, err := json.Marshal(m); err == nil {
		return b
	}
	return body
}
