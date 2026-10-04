package storage

import (
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func intPtr(v int) *int           { return &v }
func floatPtr(v float64) *float64 { return &v }
func timePtr(v time.Time) *time.Time { return &v }

// ut is a fixed, second-aligned UTC timestamp for tests that don't depend on
// "now" (round-trips, filters, aggregations). Timestamps are stored at second
// resolution, so tests build inputs the same way to keep comparisons exact.
func ut(hour, min int) time.Time {
	return time.Date(2026, 3, 10, hour, min, 0, 0, time.UTC)
}

// ago returns a second-aligned UTC timestamp relative to real now — window
// derivation compares events against `now`, so window tests must control that
// relationship.
func ago(mins int) time.Time {
	return time.Now().UTC().Add(-time.Duration(mins) * time.Minute).Truncate(time.Second)
}

func mkUsageEvent(provider string, at time.Time) *UsageEvent {
	return &UsageEvent{
		CreatedAt: at,
		Provider:  provider,
		ModelUsed: "glm-4.7",
		ModelAsked: "claude-sonnet-4-6",
		Status:    200,
		Success:   true,
		Attempt:   1,
	}
}

// ─── U1: record / fetch / NULL semantics ───────────────────────────────────

func TestUsageEventRoundTrip(t *testing.T) {
	db := newTestDB(t)
	at := ut(10, 0)
	reset := at.Add(2 * time.Hour)
	e := &UsageEvent{
		CreatedAt:  at,
		Provider:   "zai",
		ModelUsed:  "glm-5.2",
		ModelAsked: "claude-sonnet-4-6",
		RequestID:  "req_abc",
		KeyIndex:   intPtr(2),
		Attempt:    1,
		Status:     200,
		Success:    true,
		Stream:     true,
		In:         intPtr(1000),
		Out:        intPtr(500),
		CacheRead:  intPtr(2000),
		CacheWrite: intPtr(1500),
		Reasoning:  intPtr(42),
		DurationMS: 1234,
		QuotaDimension:  "5h",
		QuotaUtilization: floatPtr(0.42),
		QuotaResetAt:     timePtr(reset),
		QuotaMeta:        `{"anthropic-ratelimit-unified-status":"allowed"}`,
	}
	id, err := db.RecordUsageEvent(e)
	if err != nil {
		t.Fatal(err)
	}
	if id <= 0 {
		t.Fatalf("expected positive id, got %d", id)
	}

	got, err := db.GetUsageEvents(UsageFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	ev := got[0]
	if ev.Provider != "zai" || ev.ModelUsed != "glm-5.2" || ev.ModelAsked != "claude-sonnet-4-6" {
		t.Errorf("identity fields wrong: %+v", ev)
	}
	if ev.RequestID != "req_abc" || ev.KeyIndex == nil || *ev.KeyIndex != 2 || ev.Attempt != 1 {
		t.Errorf("attempt fields wrong: %+v", ev)
	}
	if !ev.Success || !ev.Stream || ev.Status != 200 || ev.DurationMS != 1234 {
		t.Errorf("status fields wrong: %+v", ev)
	}
	if ev.In == nil || *ev.In != 1000 || ev.Out == nil || *ev.Out != 500 {
		t.Errorf("token fields wrong: in=%v out=%v", ev.In, ev.Out)
	}
	if ev.CacheRead == nil || *ev.CacheRead != 2000 || ev.CacheWrite == nil || *ev.CacheWrite != 1500 {
		t.Errorf("cache fields wrong: %+v", ev)
	}
	if ev.Reasoning == nil || *ev.Reasoning != 42 {
		t.Errorf("reasoning wrong: %+v", ev.Reasoning)
	}
	if ev.QuotaDimension != "5h" || ev.QuotaUtilization == nil || *ev.QuotaUtilization != 0.42 {
		t.Errorf("quota fields wrong: %+v", ev)
	}
	if ev.QuotaResetAt == nil || !ev.QuotaResetAt.Equal(reset) {
		t.Errorf("quota reset wrong: %v", ev.QuotaResetAt)
	}
	if ev.QuotaMeta == "" {
		t.Errorf("quota meta lost")
	}
	if ev.Error != "" || ev.UsagePartial || ev.RateLimited || ev.Probe {
		t.Errorf("flags should be clear: %+v", ev)
	}
}

func TestUsageEventNullSemantics(t *testing.T) {
	db := newTestDB(t)
	// Minimal event: only identity fields — everything else must come back
	// as NULL (nil pointers), never as fabricated zeros.
	if _, err := db.RecordUsageEvent(&UsageEvent{
		CreatedAt: ut(10, 0), Provider: "ollama", ModelUsed: "qwen3:8b", Status: 200, Success: true,
	}); err != nil {
		t.Fatal(err)
	}
	// A zero REPORTED value (0 tokens) must survive as 0, not become NULL.
	if _, err := db.RecordUsageEvent(&UsageEvent{
		CreatedAt: ut(10, 1), Provider: "ollama", ModelUsed: "qwen3:8b", Status: 200, Success: true,
		In: intPtr(0), Out: intPtr(0),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetUsageEvents(UsageFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 events, got %d", len(got))
	}
	// Default ordering is most recent first: got[0] = the zeros event.
	if got[0].In == nil || *got[0].In != 0 || got[0].Out == nil || *got[0].Out != 0 {
		t.Errorf("reported zeros must stay 0: %+v", got[0])
	}
	// got[1] = the minimal event: missing fields stay NULL.
	if got[1].In != nil || got[1].Out != nil || got[1].CacheRead != nil || got[1].CacheWrite != nil ||
		got[1].Reasoning != nil || got[1].KeyIndex != nil || got[1].QuotaDimension != "" {
		t.Errorf("missing fields must stay NULL: %+v", got[1])
	}
}

func TestUsageEventRateLimitedRoundTrip(t *testing.T) {
	db := newTestDB(t)
	at := ut(10, 0)
	retryReset := at.Add(30 * time.Second)
	_, err := db.RecordUsageEvent(&UsageEvent{
		CreatedAt: at, Provider: "anthropic", ModelUsed: "claude-sonnet-4-6",
		Status: 429, Success: false, RateLimited: true,
		RetryAfter:   floatPtr(30),
		RetryResetAt: timePtr(retryReset),
		Error:        "rate limited",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.GetUsageEvents(UsageFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].RateLimited || got[0].Status != 429 {
		t.Fatalf("rate-limit fields lost: %+v", got)
	}
	if got[0].RetryAfter == nil || *got[0].RetryAfter != 30 {
		t.Errorf("retry-after lost: %+v", got[0].RetryAfter)
	}
	if got[0].RetryResetAt == nil || !got[0].RetryResetAt.Equal(retryReset) {
		t.Errorf("retry reset lost: %v", got[0].RetryResetAt)
	}
	if got[0].Error != "rate limited" {
		t.Errorf("error text lost: %q", got[0].Error)
	}
}

func TestUsageEventMigrationFromOldSchema(t *testing.T) {
	// Simulate a pre-feature database: only the requests-era schema, with data.
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	oldSchema := `
	CREATE TABLE IF NOT EXISTS requests (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		request_id    TEXT,
		model_asked   TEXT NOT NULL,
		model_used    TEXT NOT NULL,
		provider      TEXT NOT NULL,
		complexity    TEXT NOT NULL,
		input_tokens  INTEGER DEFAULT 0,
		output_tokens INTEGER DEFAULT 0,
		cache_read_tokens  INTEGER DEFAULT 0,
		cache_write_tokens INTEGER DEFAULT 0,
		cost_usd      REAL DEFAULT 0,
		cache_saved_usd REAL DEFAULT 0,
		latency_ms    INTEGER DEFAULT 0,
		status        INTEGER DEFAULT 200,
		error         TEXT,
		stream        BOOLEAN DEFAULT FALSE
	);`
	if _, err := raw.Exec(oldSchema); err != nil {
		t.Fatal(err)
	}
	// Note: error and request_id seeded as '' (not NULL) — GetRecentRequests
	// scans them into plain strings, matching how LogRequest always writes them.
	if _, err := raw.Exec(`INSERT INTO requests (created_at, request_id, model_asked, model_used, provider, complexity, input_tokens, output_tokens, error)
		VALUES ('2026-03-01 10:00:00', '', 'claude-haiku-4-5', 'llama-3.3-70b-versatile', 'groq', 'simple', 10, 5, '')`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	// Reopen through New() — migrate() must add usage_events and keep old data.
	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	reqs, err := db.GetRecentRequests(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 1 || reqs[0].Provider != "groq" || reqs[0].InputTokens != 10 {
		t.Fatalf("pre-existing requests data damaged: %+v", reqs)
	}
	if _, err := db.RecordUsageEvent(mkUsageEvent("groq", ut(10, 0))); err != nil {
		t.Fatalf("usage_events table missing after migration: %v", err)
	}
}

func TestUsageEventFilters(t *testing.T) {
	db := newTestDB(t)
	seed := []struct {
		p, m  string
		rl    bool
		probe bool
	}{
		{"zai", "glm-4.7", false, false},
		{"zai", "glm-5.2", false, false},
		{"zai", "glm-4.7", true, false},
		{"anthropic", "claude-sonnet-4-6", false, false},
		{"zai", "glm-4.7", false, true}, // probe
	}
	for i, s := range seed {
		e := mkUsageEvent(s.p, ut(10, i))
		e.ModelUsed = s.m
		e.RateLimited = s.rl
		e.Probe = s.probe
		if s.probe {
			e.Success, e.Status = true, 200
		}
		if _, err := db.RecordUsageEvent(e); err != nil {
			t.Fatal(err)
		}
	}

	all, err := db.GetUsageEvents(UsageFilter{Limit: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 5 {
		t.Fatalf("unfiltered (probes included) should return all 5, got %d", len(all))
	}

	// Default feed ordering is most recent first.
	if !all[0].CreatedAt.After(all[4].CreatedAt) {
		t.Errorf("expected DESC ordering, got %v .. %v", all[0].CreatedAt, all[4].CreatedAt)
	}

	asc, err := db.GetUsageEvents(UsageFilter{Ascending: true, Limit: 0})
	if err != nil {
		t.Fatal(err)
	}
	if !asc[0].CreatedAt.Before(asc[4].CreatedAt) {
		t.Errorf("Ascending ordering broken")
	}

	zai, err := db.GetUsageEvents(UsageFilter{Provider: "zai", Limit: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(zai) != 4 {
		t.Fatalf("provider filter: want 4 zai events, got %d", len(zai))
	}

	glm47, err := db.GetUsageEvents(UsageFilter{Provider: "zai", Model: "glm-4.7", Limit: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(glm47) != 3 {
		t.Fatalf("model filter: want 3 glm-4.7 events, got %d", len(glm47))
	}

	rl, err := db.GetUsageEvents(UsageFilter{RateLimitedOnly: true, Limit: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(rl) != 1 || !rl[0].RateLimited {
		t.Fatalf("rate-limit-only filter wrong: %+v", rl)
	}

	noProbes, err := db.GetUsageEvents(UsageFilter{ExcludeProbes: true, Limit: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(noProbes) != 4 {
		t.Fatalf("ExcludeProbes: want 4, got %d", len(noProbes))
	}

	lim, err := db.GetUsageEvents(UsageFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(lim) != 2 {
		t.Fatalf("limit: want 2, got %d", len(lim))
	}

	since := ut(10, 1)
	inRange, err := db.GetUsageEvents(UsageFilter{Since: since, Limit: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(inRange) != 4 {
		t.Fatalf("since filter: want 4 (10:01 and later), got %d", len(inRange))
	}
	until := ut(10, 3)
	before, err := db.GetUsageEvents(UsageFilter{Until: until, Limit: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 3 {
		t.Fatalf("until filter: want 3 (before 10:03), got %d", len(before))
	}
}

func TestUsageEventsConcurrent(t *testing.T) {
	db := newTestDB(t)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				e := mkUsageEvent("zai", ut(10, i))
				e.In = intPtr(i)
				if _, err := db.RecordUsageEvent(e); err != nil {
					t.Errorf("concurrent record: %v", err)
					return
				}
				if _, err := db.GetUsageEvents(UsageFilter{Limit: 5}); err != nil {
					t.Errorf("concurrent read: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	all, err := db.GetUsageEvents(UsageFilter{Limit: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 200 {
		t.Fatalf("want 200 recorded events, got %d", len(all))
	}
}

// ─── U2: window derivation ──────────────────────────────────────────────────

func recordAt(t *testing.T, db *DB, provider string, at time.Time, mut ...func(*UsageEvent)) {
	t.Helper()
	e := mkUsageEvent(provider, at)
	for _, m := range mut {
		m(e)
	}
	if _, err := db.RecordUsageEvent(e); err != nil {
		t.Fatal(err)
	}
}

func TestUsageWindowsOpenCurrent(t *testing.T) {
	db := newTestDB(t)
	recordAt(t, db, "zai", ago(40), func(e *UsageEvent) {
		e.In, e.CacheRead, e.CacheWrite, e.Out = intPtr(1000), intPtr(3000), intPtr(500), intPtr(200)
	})
	recordAt(t, db, "zai", ago(30), func(e *UsageEvent) {
		e.In, e.CacheRead, e.Out = intPtr(500), intPtr(2000), intPtr(100)
		e.ModelUsed = "glm-5.2"
	})

	ws, err := db.GetUsageWindows("zai", 5*time.Hour, "5h", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 {
		t.Fatalf("expected 1 window, got %d", len(ws))
	}
	w := ws[0]
	if w.EndedAt != nil || w.EndReason != "" {
		t.Errorf("recent activity should leave the window open, got end=%v reason=%q", w.EndedAt, w.EndReason)
	}
	if w.Requests != 2 {
		t.Errorf("requests: want 2, got %d", w.Requests)
	}
	if w.InTokens != 1500 || w.CacheReadTokens != 5000 || w.CacheWriteTokens != 500 || w.OutTokens != 300 {
		t.Errorf("token totals wrong: %+v", w)
	}
	if w.TotalInputTokens != 7000 || w.TotalTokens != 7300 {
		t.Errorf("derived totals wrong: in=%d total=%d", w.TotalInputTokens, w.TotalTokens)
	}
	if w.CacheHitRatio < 0.714 || w.CacheHitRatio > 0.715 {
		t.Errorf("cache hit ratio: want ~0.7143, got %v", w.CacheHitRatio)
	}
	if len(w.PerModel) != 2 {
		t.Fatalf("per-model breakdown: want 2, got %+v", w.PerModel)
	}
	// glm-4.7 has the larger total input volume — breakdown sorted by volume desc.
	if w.PerModel[0].Model != "glm-4.7" || w.PerModel[0].Requests != 1 {
		t.Errorf("per-model order wrong: %+v", w.PerModel)
	}
}

func TestUsageWindowsTimeElapsedTermination(t *testing.T) {
	db := newTestDB(t)
	// base is 10h ago; the 5h wall falls 5h ago.
	base := ago(600)
	recordAt(t, db, "zai", base, func(e *UsageEvent) { e.In = intPtr(100) })
	recordAt(t, db, "zai", base.Add(2*time.Hour), func(e *UsageEvent) { e.In = intPtr(200) })
	// Event 6h after base (4h ago) lands past base+5h → boundary at base+5h.
	recordAt(t, db, "zai", base.Add(6*time.Hour), func(e *UsageEvent) { e.In = intPtr(300) })

	ws, err := db.GetUsageWindows("zai", 5*time.Hour, "5h", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 {
		t.Fatalf("expected 2 windows, got %d", len(ws))
	}
	first, second := ws[1], ws[0] // most recent first
	// Oldest window: its TIME_ELAPSED end is start-relative and the true start
	// predates recorded history → UNKNOWN.
	if first.EndReason != "UNKNOWN" {
		t.Errorf("oldest window: want UNKNOWN (start-relative end), got %q", first.EndReason)
	}
	if first.EndedAt == nil || !first.EndedAt.Equal(base.Add(5*time.Hour)) {
		t.Errorf("oldest window should end at base+5h, got %v", first.EndedAt)
	}
	if first.Requests != 2 || first.InTokens != 300 {
		t.Errorf("oldest window contents wrong: %+v", first)
	}
	// The event past the wall starts a fresh, still-open window (4h old).
	if second.EndReason != "" || second.EndedAt != nil {
		t.Errorf("recent window should be open, got reason=%q end=%v", second.EndReason, second.EndedAt)
	}
	if second.Requests != 1 || second.InTokens != 300 {
		t.Errorf("recent window contents wrong: %+v", second)
	}
}

func TestUsageWindowsRateLimitTermination(t *testing.T) {
	db := newTestDB(t)
	base := ago(180) // 3h ago
	recordAt(t, db, "zai", base, func(e *UsageEvent) { e.In = intPtr(100) })
	recordAt(t, db, "zai", base.Add(time.Hour), func(e *UsageEvent) { e.In = intPtr(200) })
	// A 429 closes the window AT its own timestamp and belongs to that window.
	rlAt := base.Add(2 * time.Hour)
	recordAt(t, db, "zai", rlAt, func(e *UsageEvent) {
		e.Status, e.Success, e.RateLimited = 429, false, true
		e.RetryAfter = floatPtr(30)
	})
	// Recovery + new activity → new window.
	recordAt(t, db, "zai", base.Add(150*time.Minute), func(e *UsageEvent) { e.In = intPtr(50) })

	ws, err := db.GetUsageWindows("zai", 5*time.Hour, "5h", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 {
		t.Fatalf("expected 2 windows, got %d", len(ws))
	}
	limited, next := ws[1], ws[0]
	// RATE_LIMIT ends are event-derived facts — kept even on the oldest window.
	if limited.EndReason != "RATE_LIMIT" {
		t.Errorf("want RATE_LIMIT, got %q", limited.EndReason)
	}
	if limited.EndedAt == nil || !limited.EndedAt.Equal(rlAt) {
		t.Errorf("RATE_LIMIT window should end at the 429 timestamp, got %v", limited.EndedAt)
	}
	// The 429 belongs to the window it terminates.
	if limited.Requests != 3 {
		t.Errorf("rate-limited event should be counted in the window it terminates: %+v", limited)
	}
	if limited.RateLimitedEvents != 1 {
		t.Errorf("rate-limited count wrong: %d", limited.RateLimitedEvents)
	}
	if limited.InTokens != 300 {
		t.Errorf("window totals should sum only consuming events: %d", limited.InTokens)
	}
	if next.Requests != 1 || next.InTokens != 50 {
		t.Errorf("next window wrong: %+v", next)
	}
}

func TestUsageWindowsConsecutiveRateLimitsDoNotOpenWindows(t *testing.T) {
	db := newTestDB(t)
	base := ago(180)
	recordAt(t, db, "zai", base, func(e *UsageEvent) { e.In = intPtr(100) })
	recordAt(t, db, "zai", base.Add(time.Hour), func(e *UsageEvent) {
		e.Status, e.Success, e.RateLimited = 429, false, true
	})
	recordAt(t, db, "zai", base.Add(61*time.Minute), func(e *UsageEvent) {
		e.Status, e.Success, e.RateLimited = 429, false, true
	})
	recordAt(t, db, "zai", base.Add(62*time.Minute), func(e *UsageEvent) {
		e.Status, e.Success, e.RateLimited = 429, false, true
	})
	// First success after the burst starts the next window.
	recordAt(t, db, "zai", base.Add(2*time.Hour), func(e *UsageEvent) { e.In = intPtr(500) })

	ws, err := db.GetUsageWindows("zai", 5*time.Hour, "5h", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 {
		t.Fatalf("a 429 burst must not open windows: got %d", len(ws))
	}
	limited, next := ws[1], ws[0]
	if limited.EndReason != "RATE_LIMIT" || limited.Requests != 2 {
		t.Errorf("first window wrong: %+v", limited)
	}
	if next.Requests != 1 || next.InTokens != 500 {
		t.Errorf("recovery window wrong: %+v", next)
	}
}

func TestUsageWindowsProviderResetTermination(t *testing.T) {
	db := newTestDB(t)
	base := ago(180)
	reset := base.Add(2 * time.Hour)
	// The provider reports the 5h quota reset epoch (dimension "5h").
	recordAt(t, db, "anthropic", base, func(e *UsageEvent) {
		e.In = intPtr(100)
		e.QuotaDimension, e.QuotaUtilization, e.QuotaResetAt = "5h", floatPtr(0.5), timePtr(reset)
	})
	recordAt(t, db, "anthropic", base.Add(time.Hour), func(e *UsageEvent) { e.In = intPtr(100) })
	// Event after the reported reset → boundary at the reset epoch itself.
	recordAt(t, db, "anthropic", base.Add(150*time.Minute), func(e *UsageEvent) { e.In = intPtr(700) })

	ws, err := db.GetUsageWindows("anthropic", 5*time.Hour, "5h", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 {
		t.Fatalf("expected 2 windows, got %d", len(ws))
	}
	first, second := ws[1], ws[0]
	// PROVIDER_RESET ends are provider-reported epochs — kept on the oldest window.
	if first.EndReason != "PROVIDER_RESET" {
		t.Errorf("want PROVIDER_RESET, got %q", first.EndReason)
	}
	if first.EndedAt == nil || !first.EndedAt.Equal(reset) {
		t.Errorf("PROVIDER_RESET should end at the reported epoch, got %v", first.EndedAt)
	}
	if first.Requests != 2 || first.InTokens != 200 {
		t.Errorf("first window contents wrong: %+v", first)
	}
	if first.Quota == nil || first.Quota.Utilization != 0.5 {
		t.Errorf("window should carry the observed quota: %+v", first.Quota)
	}
	if second.Requests != 1 || second.InTokens != 700 {
		t.Errorf("second window wrong: %+v", second)
	}
}

func TestUsageWindowsUnknownOldestWindow(t *testing.T) {
	db := newTestDB(t)
	// Oldest window closes via the start-relative 5h wall (→ UNKNOWN);
	// the second closes via a 429 (→ RATE_LIMIT, a fact).
	base := ago(600)
	recordAt(t, db, "zai", base, func(e *UsageEvent) { e.In = intPtr(100) })
	recordAt(t, db, "zai", base.Add(time.Hour), func(e *UsageEvent) { e.In = intPtr(100) })
	recordAt(t, db, "zai", base.Add(6*time.Hour), func(e *UsageEvent) { e.In = intPtr(300) })
	rlAt := base.Add(7 * time.Hour)
	recordAt(t, db, "zai", rlAt, func(e *UsageEvent) {
		e.Status, e.Success, e.RateLimited = 429, false, true
	})

	ws, err := db.GetUsageWindows("zai", 5*time.Hour, "5h", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 {
		t.Fatalf("expected 2 windows, got %d", len(ws))
	}
	if ws[1].EndReason != "UNKNOWN" {
		t.Errorf("oldest window (start-relative end) must be UNKNOWN, got %q", ws[1].EndReason)
	}
	if ws[1].EndedAt == nil || !ws[1].EndedAt.Equal(base.Add(5*time.Hour)) {
		t.Errorf("UNKNOWN window must still be closed at the assumed wall: %v", ws[1].EndedAt)
	}
	if ws[0].EndReason != "RATE_LIMIT" || ws[0].Requests != 2 {
		t.Errorf("second window should keep its event-derived reason: %+v", ws[0])
	}
}

func TestUsageWindowsProbesExcluded(t *testing.T) {
	db := newTestDB(t)
	recordAt(t, db, "zai", ago(30), func(e *UsageEvent) { e.In = intPtr(100) })
	recordAt(t, db, "zai", ago(29), func(e *UsageEvent) { e.Probe = true; e.Success, e.Status = true, 200 })
	recordAt(t, db, "zai", ago(28), func(e *UsageEvent) { e.Probe = true; e.Success, e.Status = true, 200 })

	ws, err := db.GetUsageWindows("zai", 5*time.Hour, "5h", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 || ws[0].Requests != 1 {
		t.Fatalf("probe events must not appear in windows: %+v", ws)
	}
}

func TestUsageWindowsPartialStreamsCounted(t *testing.T) {
	db := newTestDB(t)
	recordAt(t, db, "zai", ago(30), func(e *UsageEvent) { e.In = intPtr(100) })
	recordAt(t, db, "zai", ago(25), func(e *UsageEvent) {
		e.UsagePartial = true
		e.In, e.Out = intPtr(200), intPtr(10)
	})

	ws, err := db.GetUsageWindows("zai", 5*time.Hour, "5h", 10)
	if err != nil {
		t.Fatal(err)
	}
	if ws[0].PartialEvents != 1 || ws[0].InTokens != 300 || ws[0].OutTokens != 10 {
		t.Errorf("partial events must contribute their captured usage: %+v", ws[0])
	}
}

func TestUsageWindowsLimitMostRecentFirst(t *testing.T) {
	db := newTestDB(t)
	base := ago(480) // 8h ago
	// w1: base → 429 at base+2.5h (RATE_LIMIT)
	for _, off := range []time.Duration{0, time.Hour, 2 * time.Hour} {
		recordAt(t, db, "zai", base.Add(off), func(e *UsageEvent) { e.In = intPtr(100) })
	}
	recordAt(t, db, "zai", base.Add(150*time.Minute), func(e *UsageEvent) {
		e.Status, e.Success, e.RateLimited = 429, false, true
	})
	// w2: base+4h → 429 at base+4.5h (RATE_LIMIT)
	recordAt(t, db, "zai", base.Add(4*time.Hour), func(e *UsageEvent) { e.In = intPtr(200) })
	recordAt(t, db, "zai", base.Add(270*time.Minute), func(e *UsageEvent) {
		e.Status, e.Success, e.RateLimited = 429, false, true
	})
	// w3: base+7h (1h ago) → open
	recordAt(t, db, "zai", base.Add(7*time.Hour), func(e *UsageEvent) { e.In = intPtr(300) })

	ws, err := db.GetUsageWindows("zai", 5*time.Hour, "5h", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 {
		t.Fatalf("limit: want 2, got %d", len(ws))
	}
	if ws[0].InTokens != 300 || ws[0].EndReason != "" || ws[0].EndedAt != nil {
		t.Errorf("most recent window should be first and open: %+v", ws[0])
	}
	if ws[1].InTokens != 200 || ws[1].EndReason != "RATE_LIMIT" || ws[1].Requests != 2 {
		t.Errorf("second window wrong: %+v", ws[1])
	}
}

// ─── U2: aggregations ───────────────────────────────────────────────────────

func TestUsageTotals(t *testing.T) {
	db := newTestDB(t)
	recordAt(t, db, "zai", ut(10, 0), func(e *UsageEvent) {
		e.ModelUsed = "glm-4.7"
		e.In, e.CacheRead, e.Out = intPtr(1000), intPtr(2000), intPtr(100)
	})
	recordAt(t, db, "zai", ut(10, 1), func(e *UsageEvent) {
		e.ModelUsed = "glm-5.2"
		e.In, e.Out, e.Reasoning = intPtr(500), intPtr(50), intPtr(20)
	})
	recordAt(t, db, "anthropic", ut(10, 2), func(e *UsageEvent) {
		e.ModelUsed = "claude-sonnet-4-6"
		e.In, e.Out = intPtr(200), intPtr(30)
	})
	// An event with NULL usage must not fabricate zeros.
	recordAt(t, db, "anthropic", ut(10, 3), func(e *UsageEvent) {
		e.ModelUsed = "claude-sonnet-4-6"
	})
	// Probes never count toward usage totals.
	recordAt(t, db, "anthropic", ut(10, 4), func(e *UsageEvent) {
		e.Probe, e.Success, e.Status = true, true, 200
	})

	byProvider, err := db.GetUsageTotals("provider", "all")
	if err != nil {
		t.Fatal(err)
	}
	pm := map[string]*UsageTotals{}
	for _, p := range byProvider {
		pm[p.Group] = p
	}
	if pm["zai"].Requests != 2 || pm["zai"].In != 1500 || pm["zai"].CacheRead != 2000 ||
		pm["zai"].Out != 150 || pm["zai"].Reasoning != 20 {
		t.Errorf("zai totals wrong: %+v", pm["zai"])
	}
	if pm["anthropic"].Requests != 2 || pm["anthropic"].In != 200 || pm["anthropic"].Out != 30 {
		t.Errorf("anthropic totals wrong (NULL usage event counted as request but not tokens): %+v", pm["anthropic"])
	}

	byModel, err := db.GetUsageTotals("model", "all")
	if err != nil {
		t.Fatal(err)
	}
	mm := map[string]*UsageTotals{}
	for _, m := range byModel {
		mm[m.Group] = m
	}
	if mm["glm-4.7"].In != 1000 || mm["glm-5.2"].Reasoning != 20 || mm["claude-sonnet-4-6"].Requests != 2 {
		t.Errorf("model totals wrong: %+v", byModel)
	}
}

func TestGetProviderQuota(t *testing.T) {
	db := newTestDB(t)
	recordAt(t, db, "anthropic", ut(10, 0), func(e *UsageEvent) {
		e.QuotaDimension, e.QuotaUtilization, e.QuotaResetAt = "5h", floatPtr(0.25), timePtr(ut(12, 0))
	})
	recordAt(t, db, "anthropic", ut(10, 30), func(e *UsageEvent) {
		e.QuotaDimension, e.QuotaUtilization, e.QuotaResetAt = "5h", floatPtr(0.5), timePtr(ut(12, 0))
	})
	recordAt(t, db, "anthropic", ut(10, 40), func(e *UsageEvent) {
		e.QuotaDimension, e.QuotaUtilization = "7d", floatPtr(0.9)
	})
	// Unlabeled quota info gets its own snapshot (dimension "").
	recordAt(t, db, "zai", ut(10, 45), func(e *UsageEvent) {
		e.QuotaUtilization = floatPtr(0.7)
	})

	snaps, err := db.GetProviderQuota()
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 3 {
		t.Fatalf("want 3 snapshots (anthropic 5h + 7d, zai unlabeled), got %+v", snaps)
	}
	sm := map[string]*QuotaSnapshot{}
	for _, s := range snaps {
		sm[s.Provider+"/"+s.Dimension] = s
	}
	// Latest observation per (provider, dimension) wins.
	if s := sm["anthropic/5h"]; s == nil || s.Utilization != 0.5 || s.ResetAt == nil {
		t.Errorf("5h snapshot should be the latest observation (0.5): %+v", s)
	}
	if s := sm["anthropic/7d"]; s == nil || s.Utilization != 0.9 {
		t.Errorf("7d snapshot wrong: %+v", s)
	}
	if s := sm["zai/"]; s == nil || s.Utilization != 0.7 {
		t.Errorf("unlabeled snapshot wrong: %+v", s)
	}
}