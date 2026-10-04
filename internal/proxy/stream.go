package proxy

import (
	"bytes"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/lynuxis2026-pixel/nexus-proxy/internal/router"
)

var (
	reInputTokens  = regexp.MustCompile(`"input_tokens":\s*(\d+)`)
	reOutputTokens = regexp.MustCompile(`"output_tokens":\s*(\d+)`)
)

// relayAnthropicStream proxies a native-Anthropic streaming (SSE) response
// straight through to Claude Code, flushing each chunk as it arrives. It also
// captures the stream so token usage can be logged after completion.
func (h *Handler) relayAnthropicStream(w http.ResponseWriter, r *http.Request, active *activeProvider, req AnthropicRequest, resp *http.Response, startTime time.Time, complexity router.Complexity, att *attemptInfo) {
	defer resp.Body.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.writeError(w, http.StatusInternalServerError, "streaming not supported by server")
		return
	}

	copyResponseHeaders(w.Header(), resp.Header)
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "text/event-stream")
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering
	w.Header().Set("X-Nexus-Provider", active.impl.Name())
	w.WriteHeader(resp.StatusCode)
	flusher.Flush()

	var captured bytes.Buffer
	buf := make([]byte, 4096)
	// abort records the partial stream and marks both records; the captured
	// bytes hold whatever usage blocks arrived before the break
	abort := func(abortText string) {
		raw := streamRawUsage(captured.Bytes())
		h.logAbortedStream(active, req, complexity, raw.anthropicTokens(), captured.Bytes(), resp.StatusCode, startTime, att, raw, abortText)
	}
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if captured.Len() < 1<<20 { // cap capture at 1MB
				captured.Write(buf[:n])
			}
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				log.Warn().Err(writeErr).Msg("Client disconnected during stream")
				abort(abortClientGone)
				return
			}
			flusher.Flush()
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			log.Error().Err(readErr).Msg("Stream read error")
			abort(abortUpstreamErr)
			return
		}

		// Stop early if the client went away.
		select {
		case <-r.Context().Done():
			log.Debug().Msg("Client context cancelled, ending stream")
			abort(abortClientGone)
			return
		default:
		}
	}

	raw := streamRawUsage(captured.Bytes())
	u := raw.anthropicTokens()
	h.logResult(active, req, complexity, u, captured.Bytes(), resp.StatusCode, time.Since(startTime), true, att, raw)
	log.Info().
		Str("provider", active.impl.Name()).
		Int("status", resp.StatusCode).
		Int("in", u.In).
		Int("out", u.Out).
		Int("cache_read", u.CacheRead).
		Int64("latency_ms", time.Since(startTime).Milliseconds()).
		Bool("stream", true).
		Msg("Stream completed")
}

// parseStreamUsage scrapes token counts from a captured Anthropic SSE stream.
// input_tokens appears once (message_start); output_tokens appears multiple
// times, so we take the final (largest-progress) value.
func parseStreamUsage(data []byte) (in, out int) {
	if m := reInputTokens.FindSubmatch(data); m != nil {
		in, _ = strconv.Atoi(string(m[1]))
	}
	if all := reOutputTokens.FindAllSubmatch(data, -1); len(all) > 0 {
		out, _ = strconv.Atoi(string(all[len(all)-1][1]))
	}
	return in, out
}
