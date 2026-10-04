package storage

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Request represents a logged proxy request
type Request struct {
	ID               int64
	CreatedAt        time.Time
	RequestID        string
	ModelAsked       string
	ModelUsed        string
	Provider         string
	Complexity       string
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	CostUSD          float64
	CacheSavedUSD    float64
	LatencyMS        int64
	Status           int
	Error            string
	Stream           bool
	Prompt           string // full request JSON — only stored when --inspect is on
	Response         string // full response body — only stored when --inspect is on
	User             string // team attribution (empty = unattributed)
	Redacted         int    // secrets/PII masked before this request left the machine
}

// LogRequest saves a request to the database and returns the new row ID.
func (db *DB) LogRequest(req *Request) (int64, error) {
	res, err := db.conn.Exec(`
		INSERT INTO requests (
			created_at, request_id, model_asked, model_used,
			provider, complexity, input_tokens, output_tokens,
			cache_read_tokens, cache_write_tokens,
			cost_usd, cache_saved_usd, latency_ms, status, error, stream,
			prompt, response, user, redacted
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		// Store as SQLite-native UTC datetime text so date()/strftime() work.
		req.CreatedAt.UTC().Format("2006-01-02 15:04:05"),
		req.RequestID,
		req.ModelAsked,
		req.ModelUsed,
		req.Provider,
		req.Complexity,
		req.InputTokens,
		req.OutputTokens,
		req.CacheReadTokens,
		req.CacheWriteTokens,
		req.CostUSD,
		req.CacheSavedUSD,
		req.LatencyMS,
		req.Status,
		req.Error,
		req.Stream,
		req.Prompt,
		req.Response,
		req.User,
		req.Redacted,
	)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// GetRecentRequests returns the N most recent requests
func (db *DB) GetRecentRequests(limit int) ([]*Request, error) {
	rows, err := db.conn.Query(`
		SELECT id, created_at, request_id, model_asked, model_used,
			   provider, complexity, input_tokens, output_tokens,
			   cache_read_tokens, cache_write_tokens,
			   cost_usd, cache_saved_usd, latency_ms, status, error, stream
		FROM requests
		ORDER BY created_at DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanRequests(rows)
}

// GetRequestDetail returns a single request including the captured prompt and
// response (populated only when --inspect was on at the time).
func (db *DB) GetRequestDetail(id int64) (*Request, error) {
	r := &Request{}
	var created string
	err := db.conn.QueryRow(`
		SELECT id, created_at, model_asked, model_used, provider, complexity,
			   input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
			   cost_usd, cache_saved_usd, latency_ms, status, stream,
			   COALESCE(prompt,''), COALESCE(response,'')
		FROM requests WHERE id = ?`, id).Scan(
		&r.ID, &created, &r.ModelAsked, &r.ModelUsed, &r.Provider, &r.Complexity,
		&r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CacheWriteTokens,
		&r.CostUSD, &r.CacheSavedUSD, &r.LatencyMS, &r.Status, &r.Stream,
		&r.Prompt, &r.Response,
	)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// GetStats returns aggregated statistics
func (db *DB) GetStats(period string) (*Stats, error) {
	var since string
	switch period {
	case "today":
		since = "date('now')"
	case "week":
		since = "date('now', '-7 days')"
	case "month":
		since = "date('now', '-30 days')"
	default:
		since = "date('now')"
	}

	query := fmt.Sprintf(`
		SELECT
			COUNT(*) as total_requests,
			COALESCE(SUM(input_tokens), 0) as total_input_tokens,
			COALESCE(SUM(output_tokens), 0) as total_output_tokens,
			COALESCE(SUM(cost_usd), 0) as total_cost,
			COALESCE(AVG(latency_ms), 0) as avg_latency,
			COALESCE(SUM(cache_read_tokens), 0) as cache_read_tokens,
			COALESCE(SUM(cache_write_tokens), 0) as cache_write_tokens,
			COALESCE(SUM(cache_saved_usd), 0) as cache_saved_usd,
			COALESCE(SUM(redacted), 0) as redacted_total
		FROM requests
		WHERE date(created_at) >= %s`, since)

	var stats Stats
	err := db.conn.QueryRow(query).Scan(
		&stats.TotalRequests,
		&stats.TotalInputTokens,
		&stats.TotalOutputTokens,
		&stats.TotalCostUSD,
		&stats.AvgLatencyMS,
		&stats.CacheReadTokens,
		&stats.CacheWriteTokens,
		&stats.CacheSavedUSD,
		&stats.RedactedTotal,
	)
	if err != nil {
		return nil, err
	}
	stats.Period = period
	return &stats, nil
}

// GetProviderBreakdown returns stats grouped by provider
func (db *DB) GetProviderBreakdown() ([]*ProviderStats, error) {
	rows, err := db.conn.Query(`
		SELECT
			provider,
			COUNT(*) as requests,
			COALESCE(SUM(input_tokens + output_tokens), 0) as total_tokens,
			COALESCE(SUM(cost_usd), 0) as total_cost,
			COALESCE(AVG(latency_ms), 0) as avg_latency
		FROM requests
		GROUP BY provider
		ORDER BY requests DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*ProviderStats
	for rows.Next() {
		var ps ProviderStats
		if err := rows.Scan(&ps.Provider, &ps.Requests, &ps.TotalTokens, &ps.TotalCostUSD, &ps.AvgLatencyMS); err != nil {
			continue
		}
		results = append(results, &ps)
	}
	return results, nil
}

// GetCostForecast estimates monthly cost based on current usage rate
func (db *DB) GetCostForecast() (float64, error) {
	var costToday float64
	err := db.conn.QueryRow(`
		SELECT COALESCE(SUM(cost_usd), 0)
		FROM requests
		WHERE date(created_at) = date('now')`).Scan(&costToday)
	if err != nil {
		return 0, err
	}
	// Extrapolate to 30 days
	return costToday * 30, nil
}

// TimeBucket is one point in a time series.
type TimeBucket struct {
	Bucket   string  `json:"bucket"`
	Requests int     `json:"requests"`
	CostUSD  float64 `json:"cost_usd"`
}

// GetHourlySeries returns per-hour request counts and cost over the last 24h.
func (db *DB) GetHourlySeries() ([]TimeBucket, error) {
	rows, err := db.conn.Query(`
		SELECT strftime('%m-%d %H:00', created_at) AS bucket,
		       COUNT(*),
		       COALESCE(SUM(cost_usd), 0)
		FROM requests
		WHERE created_at >= datetime('now', '-24 hours')
		GROUP BY bucket
		ORDER BY bucket`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TimeBucket
	for rows.Next() {
		var b TimeBucket
		if err := rows.Scan(&b.Bucket, &b.Requests, &b.CostUSD); err != nil {
			continue
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Savings compares actual spend to what the same traffic would have cost on
// Anthropic Claude — the headline "you saved $X" metric.
type Savings struct {
	Period        string  `json:"period"`
	Requests      int     `json:"requests"`
	ActualUSD     float64 `json:"actual_usd"`
	BaselineUSD   float64 `json:"baseline_usd"`
	SavedUSD      float64 `json:"saved_usd"`
	PercentSaved  float64 `json:"percent_saved"`
	CacheSavedUSD float64 `json:"cache_saved_usd"` // portion of savings from prompt caching
}

// anthropicBaseline returns what a request would have cost on Claude, based on
// the asked model (opus/haiku exact; everything else assumes Sonnet).
func anthropicBaseline(model string, in, out int) float64 {
	var ip, op float64
	switch m := strings.ToLower(model); {
	case strings.Contains(m, "opus"):
		ip, op = 15.0, 75.0
	case strings.Contains(m, "haiku"):
		ip, op = 0.25, 1.25
	default: // sonnet-equivalent
		ip, op = 3.0, 15.0
	}
	return float64(in)/1_000_000*ip + float64(out)/1_000_000*op
}

func sinceClause(period string) string {
	switch period {
	case "week":
		return "date('now', '-7 days')"
	case "month":
		return "date('now', '-30 days')"
	default:
		return "date('now')"
	}
}

// GetSavings returns the actual vs. Claude-baseline cost for a period.
func (db *DB) GetSavings(period string) (*Savings, error) {
	q := fmt.Sprintf(`SELECT model_asked, input_tokens, output_tokens,
			cache_read_tokens, cache_write_tokens, cost_usd, cache_saved_usd
		FROM requests WHERE date(created_at) >= %s`, sinceClause(period))
	rows, err := db.conn.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	s := &Savings{Period: period}
	for rows.Next() {
		var model string
		var in, out, cr, cw int
		var cost, cacheSaved float64
		if err := rows.Scan(&model, &in, &out, &cr, &cw, &cost, &cacheSaved); err != nil {
			continue
		}
		s.Requests++
		s.ActualUSD += cost
		// On all-Claude, cached tokens would have been full-price input too.
		s.BaselineUSD += anthropicBaseline(model, in+cr+cw, out)
		s.CacheSavedUSD += cacheSaved
	}
	s.SavedUSD = s.BaselineUSD - s.ActualUSD
	if s.SavedUSD < 0 {
		s.SavedUSD = 0
	}
	if s.BaselineUSD > 0 {
		s.PercentSaved = s.SavedUSD / s.BaselineUSD * 100
	}
	return s, rows.Err()
}

// LeaderEntry is one row of the team savings leaderboard.
type LeaderEntry struct {
	User      string  `json:"user"`
	Requests  int     `json:"requests"`
	ActualUSD float64 `json:"actual_usd"`
	SavedUSD  float64 `json:"saved_usd"`
}

// GetLeaderboard returns per-user request counts, actual spend and savings vs the
// all-Claude baseline for a period, sorted by savings descending.
func (db *DB) GetLeaderboard(period string) ([]LeaderEntry, error) {
	q := fmt.Sprintf(`SELECT COALESCE(NULLIF(user,''),'unattributed') AS u,
			model_asked, input_tokens, output_tokens,
			cache_read_tokens, cache_write_tokens, cost_usd
		FROM requests WHERE date(created_at) >= %s`, sinceClause(period))
	rows, err := db.conn.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	agg := map[string]*LeaderEntry{}
	for rows.Next() {
		var u, model string
		var in, out, cr, cw int
		var cost float64
		if err := rows.Scan(&u, &model, &in, &out, &cr, &cw, &cost); err != nil {
			continue
		}
		e := agg[u]
		if e == nil {
			e = &LeaderEntry{User: u}
			agg[u] = e
		}
		e.Requests++
		e.ActualUSD += cost
		e.SavedUSD += anthropicBaseline(model, in+cr+cw, out) - cost
	}
	out := make([]LeaderEntry, 0, len(agg))
	for _, e := range agg {
		if e.SavedUSD < 0 {
			e.SavedUSD = 0
		}
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SavedUSD > out[j].SavedUSD })
	return out, rows.Err()
}

// GetComplexityBreakdown returns request counts grouped by complexity.
func (db *DB) GetComplexityBreakdown() (map[string]int, error) {
	rows, err := db.conn.Query(`SELECT complexity, COUNT(*) FROM requests GROUP BY complexity`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	m := map[string]int{}
	for rows.Next() {
		var c string
		var n int
		if err := rows.Scan(&c, &n); err == nil {
			m[c] = n
		}
	}
	return m, rows.Err()
}

// ─── Helpers ───────────────────────────────────────────────────────────────

func scanRequests(rows *sql.Rows) ([]*Request, error) {
	var results []*Request
	for rows.Next() {
		r := &Request{}
		err := rows.Scan(
			&r.ID, &r.CreatedAt, &r.RequestID, &r.ModelAsked, &r.ModelUsed,
			&r.Provider, &r.Complexity, &r.InputTokens, &r.OutputTokens,
			&r.CacheReadTokens, &r.CacheWriteTokens,
			&r.CostUSD, &r.CacheSavedUSD, &r.LatencyMS, &r.Status, &r.Error, &r.Stream,
		)
		if err != nil {
			continue
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// ─── Types ─────────────────────────────────────────────────────────────────

type Stats struct {
	Period            string  `json:"period"`
	TotalRequests     int     `json:"total_requests"`
	TotalInputTokens  int     `json:"total_input_tokens"`
	TotalOutputTokens int     `json:"total_output_tokens"`
	TotalCostUSD      float64 `json:"total_cost_usd"`
	AvgLatencyMS      float64 `json:"avg_latency_ms"`
	CacheReadTokens   int     `json:"cache_read_tokens"`
	CacheWriteTokens  int     `json:"cache_write_tokens"`
	CacheSavedUSD     float64 `json:"cache_saved_usd"`
	RedactedTotal     int     `json:"redacted_total"`
}

type ProviderStats struct {
	Provider     string  `json:"provider"`
	Requests     int     `json:"requests"`
	TotalTokens  int     `json:"total_tokens"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	AvgLatencyMS float64 `json:"avg_latency_ms"`
}
