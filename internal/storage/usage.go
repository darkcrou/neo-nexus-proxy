package storage

import (
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// ─── Usage events ───────────────────────────────────────────────────────────
//
// An immutable, append-only record of ONE completed upstream request attempt.
// Failover chain steps, key rotations, probes, and aborted streams each get
// their own event. NULL token fields mean "the provider did not report this";
// a reported zero is stored as 0. Nothing ever mutates or deletes these rows.

// UsageEvent is a single upstream attempt's measured consumption.
type UsageEvent struct {
	ID        int64     `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Provider  string    `json:"provider"`
	ModelUsed string    `json:"model_used"`
	ModelAsked string   `json:"model_asked"`

	RequestID string `json:"request_id"`
	KeyIndex  *int   `json:"key_index"` // 0-based key-pool index; nil = no pool
	Attempt   int    `json:"attempt"`   // 1-based position in the provider chain; 0 = probe

	Status  int  `json:"status"`  // upstream HTTP status; 0 = transport error
	Success bool `json:"success"`
	Stream  bool `json:"stream"`

	In         *int `json:"in_tokens"`         // fresh input tokens
	Out        *int `json:"out_tokens"`       // output tokens (reasoning included)
	CacheRead  *int `json:"cache_read_tokens"` // prompt-cache reads
	CacheWrite *int `json:"cache_write_tokens"`
	Reasoning  *int `json:"reasoning_tokens"` // subset of Out — never double-counted

	UsagePartial bool   `json:"usage_partial"` // stream aborted / usage only partially known
	DurationMS   int64  `json:"duration_ms"`
	Error        string `json:"error"`
	Probe        bool   `json:"probe"`

	RateLimited  bool      `json:"rate_limited"`
	RetryAfter   *float64  `json:"retry_after"`    // seconds
	RetryResetAt *time.Time `json:"retry_reset_at"` // absolute reset from the 429

	QuotaDimension  string    `json:"quota_dimension"`  // e.g. "5h"/"7d", transcribed from provider headers
	QuotaUtilization *float64 `json:"quota_utilization"` // 0..1 fraction, exactly as reported
	QuotaResetAt    *time.Time `json:"quota_reset_at"`    // provider-reported quota reset epoch
	QuotaMeta       string    `json:"quota_meta"`         // JSON: raw captured rate-limit/quota headers
}

// RecordUsageEvent appends one immutable usage event and returns its row ID.
func (db *DB) RecordUsageEvent(e *UsageEvent) (int64, error) {
	res, err := db.conn.Exec(`
		INSERT INTO usage_events (
			created_at, provider, model_used, model_asked,
			request_id, key_index, attempt, status, success, stream,
			in_tokens, out_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens,
			usage_partial, duration_ms, rate_limited, retry_after, retry_reset_at,
			quota_dimension, quota_utilization, quota_reset_at, quota_meta,
			error, probe
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.CreatedAt.UTC().Format("2006-01-02 15:04:05"),
		e.Provider,
		e.ModelUsed,
		e.ModelAsked,
		e.RequestID,
		nullInt(e.KeyIndex),
		e.Attempt,
		e.Status,
		e.Success,
		e.Stream,
		nullInt(e.In), nullInt(e.Out), nullInt(e.CacheRead), nullInt(e.CacheWrite), nullInt(e.Reasoning),
		e.UsagePartial, e.DurationMS, e.RateLimited,
		nullFloat(e.RetryAfter), nullTime(e.RetryResetAt),
		e.QuotaDimension, nullFloat(e.QuotaUtilization), nullTime(e.QuotaResetAt), e.QuotaMeta,
		e.Error,
		e.Probe,
	)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// UsageFilter narrows a usage-events query. The zero value returns everything,
// most recent first.
type UsageFilter struct {
	Provider string    // exact provider name
	Model    string    // exact model_used
	Since    time.Time // events at or after this time
	Until    time.Time // events strictly before this time
	Limit    int       // 0 = no limit

	RateLimitedOnly bool // only 429/rate-limited events
	ExcludeProbes   bool // drop probe (cooldown-recovery ping) events
	Ascending       bool // oldest first (window derivation); default is newest first
}

// GetUsageEvents returns usage events matching the filter.
func (db *DB) GetUsageEvents(f UsageFilter) ([]*UsageEvent, error) {
	q := `SELECT id, created_at, COALESCE(request_id,''), provider, model_used, model_asked,
			key_index, attempt, status, success, stream,
			in_tokens, out_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens,
			usage_partial, duration_ms, rate_limited, retry_after, retry_reset_at,
			COALESCE(quota_dimension,''), quota_utilization, quota_reset_at, COALESCE(quota_meta,''),
			COALESCE(error,''), probe
		FROM usage_events`
	var where []string
	var args []any
	if f.Provider != "" {
		where = append(where, "provider = ?")
		args = append(args, f.Provider)
	}
	if f.Model != "" {
		where = append(where, "model_used = ?")
		args = append(args, f.Model)
	}
	if !f.Since.IsZero() {
		where = append(where, "created_at >= ?")
		args = append(args, f.Since.UTC().Format("2006-01-02 15:04:05"))
	}
	if !f.Until.IsZero() {
		where = append(where, "created_at < ?")
		args = append(args, f.Until.UTC().Format("2006-01-02 15:04:05"))
	}
	if f.RateLimitedOnly {
		where = append(where, "rate_limited = 1")
	}
	if f.ExcludeProbes {
		where = append(where, "probe = 0")
	}
	if len(where) > 0 {
		q += " WHERE " + joinStrings(where, " AND ")
	}
	dir := "DESC"
	if f.Ascending {
		dir = "ASC"
	}
	q += fmt.Sprintf(" ORDER BY created_at %s, id %s", dir, dir)
	if f.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.Limit)
	}

	rows, err := db.conn.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanUsageEvents(rows)
}

func scanUsageEvents(rows *sql.Rows) ([]*UsageEvent, error) {
	var out []*UsageEvent
	for rows.Next() {
		var (
			e              UsageEvent
			keyIdx         sql.NullInt64
			nIn, nOut, nCR, nCW, nRS sql.NullInt64
			partial        bool
			rl             bool
			retryAfter     sql.NullFloat64
			retryReset     sql.NullTime
			util           sql.NullFloat64
			qReset         sql.NullTime
			probe          bool
		)
		err := rows.Scan(
			&e.ID, &e.CreatedAt, &e.RequestID, &e.Provider, &e.ModelUsed, &e.ModelAsked,
			&keyIdx, &e.Attempt, &e.Status, &e.Success, &e.Stream,
			&nIn, &nOut, &nCR, &nCW, &nRS,
			&partial, &e.DurationMS, &rl, &retryAfter, &retryReset,
			&e.QuotaDimension, &util, &qReset, &e.QuotaMeta,
			&e.Error, &probe,
		)
		if err != nil {
			continue
		}
		e.KeyIndex = nilInt(keyIdx)
		e.In, e.Out, e.CacheRead, e.CacheWrite, e.Reasoning = nilInt(nIn), nilInt(nOut), nilInt(nCR), nilInt(nCW), nilInt(nRS)
		e.UsagePartial, e.RateLimited, e.Probe = partial, rl, probe
		e.RetryAfter = nilFloat(retryAfter)
		e.RetryResetAt = nilTime(retryReset)
		e.QuotaUtilization = nilFloat(util)
		e.QuotaResetAt = nilTime(qReset)
		out = append(out, &e)
	}
	return out, rows.Err()
}

// ─── Window derivation ──────────────────────────────────────────────────────
//
// Quota windows are NOT persisted — they are derived at query time from the
// immutable event stream, per provider, for a parameterized duration (5h for
// the classic Claude-rate-limit window). Rules, in boundary-precedence order
// per adjacent event pair:
//
//  1. PROVIDER_RESET — a provider-reported quota reset epoch (matching the
//     requested dimension, e.g. Anthropic's unified 5h reset) observed inside
//     the window passes before the next event; the boundary is the reported
//     epoch itself. Provider-reported resets are trusted over locally
//     assumed 5h walls.
//  2. TIME_ELAPSED — the next event falls after start+duration; the boundary
//     is start+duration.
//  3. RATE_LIMIT — a rate-limited (429) event closes the window at its own
//     timestamp and is counted inside the window it terminates. A rate-limited
//     event never OPENS a window (a burst of consecutive 429s does not spawn
//     empty windows); the next non-rate-limited event starts the next window.
//
// The oldest derived window closed by TIME_ELAPSED gets end reason UNKNOWN: its
// true start predates the first recorded event, so the start-relative duration
// wall cannot be trusted there. RATE_LIMIT and PROVIDER_RESET ends are
// event-derived facts (an actual 429, a provider-reported epoch) and are kept
// even on the oldest window.
// The trailing window closes against `now` by the same rules, or stays open.

// End reasons for derived usage windows.
const (
	EndUnknown       = "UNKNOWN"        // window start predates recorded history
	EndRateLimit     = "RATE_LIMIT"     // window terminated by a 429 event
	EndProviderReset = "PROVIDER_RESET" // terminated at a provider-reported reset epoch
	EndTimeElapsed   = "TIME_ELAPSED"   // terminated by start+duration with no reset signal
)

// QuotaObservation is a provider-reported quota utilization snapshot.
type QuotaObservation struct {
	Dimension   string     `json:"dimension"`
	Utilization float64    `json:"utilization"` // 0..1 as reported
	ResetAt     *time.Time `json:"reset_at,omitempty"`
	ObservedAt  time.Time  `json:"observed_at"`
}

// ModelUsage is a per-model breakdown inside one window.
type ModelUsage struct {
	Model      string `json:"model"`
	Requests   int    `json:"requests"`
	In         int64  `json:"in_tokens"`
	CacheRead  int64  `json:"cache_read_tokens"`
	CacheWrite int64  `json:"cache_write_tokens"`
	Out        int64  `json:"out_tokens"`
	Reasoning  int64  `json:"reasoning_tokens"`
}

// UsageWindow is one derived quota window for one provider.
type UsageWindow struct {
	Provider string `json:"provider"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"` // nil = still open
	EndReason string     `json:"end_reason"`          // "" (open) or an End* constant

	Requests         int     `json:"requests"`
	InTokens         int64   `json:"in_tokens"` // fresh input
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	OutTokens        int64   `json:"out_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	TotalInputTokens int64   `json:"total_input_tokens"` // in + cache_read + cache_write
	TotalTokens      int64   `json:"total_tokens"`        // total_input + out
	CacheHitRatio    float64 `json:"cache_hit_ratio"`     // cache_read / total_input
	PartialEvents   int     `json:"partial_events"`      // aborted streams contributing partial usage
	RateLimitedEvents int    `json:"rate_limited_events"`

	Quota    *QuotaObservation `json:"quota,omitempty"` // latest provider-reported utilization inside the window
	PerModel []ModelUsage      `json:"per_model,omitempty"`
}

// GetUsageWindows derives quota windows for one provider from its full event
// history (probes excluded — they are NEXUS-internal recovery pings, not
// workload). dimension names which quota_reset_at observations to trust (e.g.
// "5h"); pass "" to disable PROVIDER_RESET detection. Returns at most limit
// windows, most recent first (limit <= 0 returns all).
func (db *DB) GetUsageWindows(provider string, window time.Duration, dimension string, limit int) ([]*UsageWindow, error) {
	evs, err := db.GetUsageEvents(UsageFilter{Provider: provider, ExcludeProbes: true, Ascending: true})
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	var windows []*UsageWindow
	var cur *UsageWindow
	var curStart time.Time
	var resetAt *time.Time // latest provider-reported reset epoch inside the current window

	closeCur := func(end time.Time, reason string) {
		cur.EndedAt = &end
		cur.EndReason = reason
		cur.finalize()
		windows = append(windows, cur)
		cur = nil
		resetAt = nil
	}

	for _, e := range evs {
		at := e.CreatedAt

		if cur != nil {
			// 1. A provider-reported reset epoch passed before this event.
			if resetAt != nil && at.After(*resetAt) {
				closeCur(*resetAt, EndProviderReset)
			}
			// 2. The assumed duration wall passed before this event.
			if cur != nil && at.After(curStart.Add(window)) {
				end := curStart.Add(window)
				closeCur(end, EndTimeElapsed)
			}
			// A rate-limited event never opens a window; after the checks
			// above, either cur is nil (this 429 follows a boundary and is
			// skipped) or it terminates cur at its own timestamp.
			if cur == nil && e.RateLimited {
				continue
			}
		}
		if cur == nil {
			if e.RateLimited {
				continue // 429s only close windows, never open them
			}
			cur = &UsageWindow{Provider: provider, StartedAt: at}
			curStart = at
			resetAt = nil
		}
		cur.add(e)
		if dimension != "" && e.QuotaDimension == dimension && e.QuotaResetAt != nil {
			resetAt = e.QuotaResetAt
		}
		if e.RateLimited {
			closeCur(at, EndRateLimit)
		}
	}

	// Trailing window: close against now by the same rules, or leave open.
	if cur != nil {
		switch {
		case resetAt != nil && now.After(*resetAt):
			closeCur(*resetAt, EndProviderReset)
		case now.After(curStart.Add(window)):
			closeCur(curStart.Add(window), EndTimeElapsed)
		default:
			cur.finalize()
			windows = append(windows, cur) // open window
		}
	}

	// A start-relative TIME_ELAPSED end on the oldest window cannot be trusted:
	// the window's true start predates recorded history.
	if len(windows) > 0 && windows[0].EndReason == EndTimeElapsed {
		windows[0].EndReason = EndUnknown
	}

	// Most recent first, bounded by limit.
	sort.Slice(windows, func(i, j int) bool { return windows[i].StartedAt.After(windows[j].StartedAt) })
	if limit > 0 && len(windows) > limit {
		windows = windows[:limit]
	}
	return windows, nil
}

func (w *UsageWindow) add(e *UsageEvent) {
	w.Requests++
	if e.In != nil {
		w.InTokens += int64(*e.In)
	}
	if e.CacheRead != nil {
		w.CacheReadTokens += int64(*e.CacheRead)
	}
	if e.CacheWrite != nil {
		w.CacheWriteTokens += int64(*e.CacheWrite)
	}
	if e.Out != nil {
		w.OutTokens += int64(*e.Out)
	}
	if e.Reasoning != nil {
		w.ReasoningTokens += int64(*e.Reasoning)
	}
	if e.UsagePartial {
		w.PartialEvents++
	}
	if e.RateLimited {
		w.RateLimitedEvents++
	}
	if e.QuotaUtilization != nil {
		w.Quota = &QuotaObservation{
			Dimension:   e.QuotaDimension,
			Utilization: *e.QuotaUtilization,
			ResetAt:     e.QuotaResetAt,
			ObservedAt:  e.CreatedAt,
		}
	}

	if w.PerModel == nil {
		w.PerModel = []ModelUsage{}
	}
	for i := range w.PerModel {
		if w.PerModel[i].Model == e.ModelUsed {
			mu := &w.PerModel[i]
			mu.Requests++
			if e.In != nil {
				mu.In += int64(*e.In)
			}
			if e.CacheRead != nil {
				mu.CacheRead += int64(*e.CacheRead)
			}
			if e.CacheWrite != nil {
				mu.CacheWrite += int64(*e.CacheWrite)
			}
			if e.Out != nil {
				mu.Out += int64(*e.Out)
			}
			if e.Reasoning != nil {
				mu.Reasoning += int64(*e.Reasoning)
			}
			return
		}
	}
	m := ModelUsage{Model: e.ModelUsed, Requests: 1}
	if e.In != nil {
		m.In = int64(*e.In)
	}
	if e.CacheRead != nil {
		m.CacheRead = int64(*e.CacheRead)
	}
	if e.CacheWrite != nil {
		m.CacheWrite = int64(*e.CacheWrite)
	}
	if e.Out != nil {
		m.Out = int64(*e.Out)
	}
	if e.Reasoning != nil {
		m.Reasoning = int64(*e.Reasoning)
	}
	w.PerModel = append(w.PerModel, m)
}

// finalize computes derived totals after all events are attached.
func (w *UsageWindow) finalize() {
	w.TotalInputTokens = w.InTokens + w.CacheReadTokens + w.CacheWriteTokens
	w.TotalTokens = w.TotalInputTokens + w.OutTokens
	if w.TotalInputTokens > 0 {
		w.CacheHitRatio = float64(w.CacheReadTokens) / float64(w.TotalInputTokens)
	}
	sort.Slice(w.PerModel, func(i, j int) bool {
		mi, mj := &w.PerModel[i], &w.PerModel[j]
		ti := mi.In + mi.CacheRead + mi.CacheWrite + mi.Out
		tj := mj.In + mj.CacheRead + mj.CacheWrite + mj.Out
		return ti > tj
	})
	if len(w.PerModel) == 0 {
		w.PerModel = nil
	}
}

// ─── Aggregations ───────────────────────────────────────────────────────────

// UsageTotals is aggregate measured consumption for one group (provider or
// model). Token sums skip NULL (unreported) events; Requests counts all
// non-probe events including 429s.
type UsageTotals struct {
	Group      string `json:"group"`
	Requests   int    `json:"requests"`
	In         int64  `json:"in_tokens"`
	CacheRead  int64  `json:"cache_read_tokens"`
	CacheWrite int64  `json:"cache_write_tokens"`
	Out        int64  `json:"out_tokens"`
	Reasoning  int64  `json:"reasoning_tokens"`
	Partial    int64  `json:"partial_events"`
}

// GetUsageTotals aggregates usage over a period ("today"/"week"/"month"/"all")
// grouped by "provider" or "model". Probes are always excluded.
func (db *DB) GetUsageTotals(by string, period string) ([]*UsageTotals, error) {
	groupCol := "provider"
	if by == "model" {
		groupCol = "model_used"
	}
	q := fmt.Sprintf(`SELECT %s, COUNT(*),
			COALESCE(SUM(in_tokens),0), COALESCE(SUM(cache_read_tokens),0),
			COALESCE(SUM(cache_write_tokens),0), COALESCE(SUM(out_tokens),0),
			COALESCE(SUM(reasoning_tokens),0), COALESCE(SUM(usage_partial),0)
		FROM usage_events
		WHERE probe = 0%s
		GROUP BY %s
		ORDER BY 1`, groupCol, usageSince(period), groupCol)

	rows, err := db.conn.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*UsageTotals
	for rows.Next() {
		t := &UsageTotals{}
		if err := rows.Scan(&t.Group, &t.Requests, &t.In, &t.CacheRead, &t.CacheWrite,
			&t.Out, &t.Reasoning, &t.Partial); err != nil {
			continue
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// QuotaSnapshot is the latest provider-reported quota utilization for one
// (provider, dimension) pair.
type QuotaSnapshot struct {
	Provider    string     `json:"provider"`
	Dimension   string     `json:"dimension"`
	Utilization float64    `json:"utilization"`
	ResetAt     *time.Time `json:"reset_at,omitempty"`
	ObservedAt  time.Time  `json:"observed_at"`
}

// GetProviderQuota returns the latest quota observation per (provider,
// dimension) — transcribed from provider responses, never computed locally.
func (db *DB) GetProviderQuota() ([]*QuotaSnapshot, error) {
	rows, err := db.conn.Query(`
		SELECT provider, COALESCE(quota_dimension,''), quota_utilization, quota_reset_at, created_at
		FROM usage_events
		WHERE quota_utilization IS NOT NULL
		  AND id IN (SELECT MAX(id) FROM usage_events
		             WHERE quota_utilization IS NOT NULL
		             GROUP BY provider, quota_dimension)
		ORDER BY provider, quota_dimension`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*QuotaSnapshot
	for rows.Next() {
		s := &QuotaSnapshot{}
		var reset sql.NullTime
		if err := rows.Scan(&s.Provider, &s.Dimension, &s.Utilization, &reset, &s.ObservedAt); err != nil {
			continue
		}
		s.ResetAt = nilTime(reset)
		out = append(out, s)
	}
	return out, rows.Err()
}

// ─── Helpers ───────────────────────────────────────────────────────────────

func usageSince(period string) string {
	switch period {
	case "all":
		return ""
	case "week":
		return " AND date(created_at) >= date('now', '-7 days')"
	case "month":
		return " AND date(created_at) >= date('now', '-30 days')"
	default: // today
		return " AND date(created_at) >= date('now')"
	}
}

func joinStrings(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}

func nullInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

func nilInt(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}

func nullFloat(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

func nilFloat(n sql.NullFloat64) *float64 {
	if !n.Valid {
		return nil
	}
	v := n.Float64
	return &v
}

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format("2006-01-02 15:04:05")
}

func nilTime(n sql.NullTime) *time.Time {
	if !n.Valid {
		return nil
	}
	t := n.Time.UTC()
	return &t
}