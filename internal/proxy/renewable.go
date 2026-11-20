// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/lynuxis2026-pixel/nexus-proxy/internal/providers"
	"github.com/lynuxis2026-pixel/nexus-proxy/internal/qwen"
)

func (h *Handler) sendToProvider(ctx context.Context, active *activeProvider, req AnthropicRequest, idx, chainPos int, probe bool, key string, retried *bool, call func(string) (*http.Response, error)) (*http.Response, error) {
	source := providers.BearerSourceFor(active.impl)
	if source == nil {
		return call(key)
	}
	start := time.Now()
	return providers.CallWithBearer(ctx, source, retried, call, func(resp *http.Response) {
		att := &attemptInfo{
			keyIdx: idx, chainPos: chainPos, probe: probe, started: start,
			quota: captureQuota(resp.Header), reqID: extractRequestID(resp.Header),
		}
		h.recordUsageEvent(active, req, req.Stream, resp.StatusCode, att, rawUsage{})
		start = time.Now()
	})
}

func (h *Handler) doProviderRequest(active *activeProvider, req *http.Request) (*http.Response, error) {
	if providers.BearerSourceFor(active.impl) != nil {
		return providers.DoRenewableRequest(h.httpClient, active.impl.BaseURL(), req)
	}
	return h.httpClient.Do(req)
}

func isQwenCredentialError(err error) bool {
	return errors.Is(err, qwen.ErrSignInRequired) || errors.Is(err, providers.ErrBearerRejected) ||
		errors.Is(err, qwen.ErrRefreshLimited) || errors.Is(err, qwen.ErrRefresh)
}

func qwenCredentialMessage(err error) string {
	if errors.Is(err, providers.ErrBearerRejected) {
		return qwen.ErrSignInRequired.Error()
	}
	return err.Error()
}

// QwenReadiness reports whether this running proxy loaded the provider and
// how many models it discovered for the supplied session revision.
func (s *Server) QwenReadiness(revision string) (loaded bool, models int) {
	h := s.handler
	if h.providers[qwen.ProviderType] == nil || h.providers[qwen.ProviderType].qwenSession == nil {
		return false, 0
	}
	h.catalog.mu.RLock()
	defer h.catalog.mu.RUnlock()
	entry := h.catalog.entries[qwen.ProviderType]
	if revision != "" && entry != nil && entry.revision == revision && entry.discovered {
		return true, len(entry.models)
	}
	return true, 0
}
