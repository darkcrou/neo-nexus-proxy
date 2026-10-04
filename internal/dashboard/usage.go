package dashboard

import (
	"net/http"
	"strconv"
	"time"

	"github.com/lynuxis2026-pixel/nexus-proxy/internal/storage"
)

// ─── /api/usage/* — usage-measurement endpoints ────────────────────────────
//
// These expose the immutable usage-event history and its derived aggregations
// (quota windows, totals, provider-reported quota). All follow the server
// conventions: snake_case JSON, plural-key envelopes, and the empty shape
// when the dashboard runs without a DB.

func parsePositiveInt(r *http.Request, key string, fallback int) int {
	if n, err := strconv.Atoi(r.URL.Query().Get(key)); err == nil && n > 0 {
		return n
	}
	return fallback
}

// handleUsageWindows returns derived quota windows (most recent first) for one
// provider, or for every provider with recorded usage when ?provider= is
// omitted. The current window has ended_at = null / end_reason = "".
//
//	GET /api/usage/windows?provider=&window=5h&dimension=5h&limit=20
func (s *Server) handleUsageWindows(w http.ResponseWriter, r *http.Request) {
	window := 5 * time.Hour
	if d, err := time.ParseDuration(r.URL.Query().Get("window")); err == nil && d > 0 {
		window = d
	}
	dimension := r.URL.Query().Get("dimension")
	if dimension == "" {
		dimension = "5h" // primary use case: Anthropic-style 5-hour windows
	}
	limit := parsePositiveInt(r, "limit", 20)

	if s.db == nil {
		writeJSON(w, map[string]interface{}{"windows": []*storage.UsageWindow{}})
		return
	}

	providers := []string{r.URL.Query().Get("provider")}
	if providers[0] == "" {
		// derive the provider list from recorded usage itself
		totals, err := s.db.GetUsageTotals("provider", "all")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		providers = providers[:0]
		for _, t := range totals {
			providers = append(providers, t.Group)
		}
	}

	windows := []*storage.UsageWindow{}
	for _, p := range providers {
		ws, err := s.db.GetUsageWindows(p, window, dimension, limit)
		if err != nil {
			// A provider whose derivation fails must be loud: returning the
			// rest would present partial data as complete (the exact silent-
			// emptiness failure mode the storage COALESCE fix addressed).
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		windows = append(windows, ws...)
	}
	writeJSON(w, map[string]interface{}{"windows": windows})
}

// handleUsageEvents returns raw usage events, newest first by default.
//
//	GET /api/usage/events?provider=&model=&since=&until=&limit=100
//	    &rate_limited=1&probes=0
//
// Probe (cooldown-recovery) events are included by default; pass probes=0 to
// exclude them. since/until are RFC3339 (until is exclusive).
func (s *Server) handleUsageEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := storage.UsageFilter{
		Provider: q.Get("provider"),
		Model:    q.Get("model"),
		Limit:    parsePositiveInt(r, "limit", 100),
	}
	if t, err := time.Parse(time.RFC3339, q.Get("since")); err == nil {
		f.Since = t
	}
	if t, err := time.Parse(time.RFC3339, q.Get("until")); err == nil {
		f.Until = t
	}
	f.RateLimitedOnly = q.Get("rate_limited") == "1" || q.Get("rate_limited") == "true"
	f.ExcludeProbes = q.Get("probes") == "0"

	empty := map[string]interface{}{"events": []*storage.UsageEvent{}}
	if s.db == nil {
		writeJSON(w, empty)
		return
	}
	events, err := s.db.GetUsageEvents(f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if events == nil {
		events = []*storage.UsageEvent{}
	}
	writeJSON(w, map[string]interface{}{"events": events})
}

// handleUsageTotals aggregates usage events grouped by provider or model.
//
//	GET /api/usage/totals?by=provider|model&period=today|week|month|all
func (s *Server) handleUsageTotals(w http.ResponseWriter, r *http.Request) {
	by := r.URL.Query().Get("by")
	if by != "model" {
		by = "provider"
	}
	period := r.URL.Query().Get("period")
	switch period {
	case "today", "week", "month", "all":
	default:
		period = "all"
	}

	empty := map[string]interface{}{"by": by, "period": period, "totals": []*storage.UsageTotals{}}
	if s.db == nil {
		writeJSON(w, empty)
		return
	}
	totals, err := s.db.GetUsageTotals(by, period)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if totals == nil {
		writeJSON(w, empty)
		return
	}
	writeJSON(w, map[string]interface{}{"by": by, "period": period, "totals": totals})
}

// handleUsageQuota returns the latest provider-reported quota utilization per
// (provider, dimension) pair, transcribed from upstream responses — never
// computed locally.
//
//	GET /api/usage/quota
func (s *Server) handleUsageQuota(w http.ResponseWriter, r *http.Request) {
	empty := map[string]interface{}{"quota": []*storage.QuotaSnapshot{}}
	if s.db == nil {
		writeJSON(w, empty)
		return
	}
	quota, err := s.db.GetProviderQuota()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if quota == nil {
		writeJSON(w, empty)
		return
	}
	writeJSON(w, map[string]interface{}{"quota": quota})
}
