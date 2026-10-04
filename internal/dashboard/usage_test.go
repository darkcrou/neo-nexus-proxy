package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/lynuxis2026-pixel/nexus-proxy/internal/storage"
)

// ─── U6: /api/usage/* endpoints ────────────────────────────────────────────
//
// The four usage endpoints expose the immutable usage-event history and the
// window/total/quota aggregations. Envelopes use plural keys with snake_case
// fields; a nil DB (dashboard without storage) yields the empty shape, matching
// every other dashboard endpoint.

func newUsageTestDB(t *testing.T) *storage.DB {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "u.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func ip(n int) *int             { return &n }
func fp(n float64) *float64     { return &n }
func tp(t time.Time) *time.Time { return &t }

func getJSON(t *testing.T, s *Server, path string) map[string]interface{} {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	if rec.Code != 200 {
		t.Fatalf("%s = %d, body %s", path, rec.Code, rec.Body.String())
	}
	var m map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("%s: bad JSON: %v", path, err)
	}
	return m
}

func TestUsageAPI_NoDB(t *testing.T) {
	s := &Server{} // no db
	for _, p := range []string{"/api/usage/windows", "/api/usage/events", "/api/usage/totals", "/api/usage/quota"} {
		m := getJSON(t, s, p)
		for _, key := range []string{"windows", "events", "totals", "quota"} {
			if v, ok := m[key]; ok {
				arr, isArr := v.([]interface{})
				if !isArr || arr == nil {
					t.Errorf("%s: %q should be an empty array, got %T %v", p, key, v, v)
				}
			}
		}
	}
}

func TestUsageAPI_Events(t *testing.T) {
	db := newUsageTestDB(t)
	now := time.Now().UTC()
	for _, e := range []*storage.UsageEvent{
		{CreatedAt: now.Add(-2 * time.Hour), Provider: "anthropic", ModelUsed: "claude-sonnet-4-6", ModelAsked: "claude-sonnet-4-6", Status: 200, Success: true, In: ip(120), Out: ip(30), CacheRead: ip(500), CacheWrite: ip(80), Reasoning: ip(10)},
		{CreatedAt: now.Add(-90 * time.Minute), Provider: "anthropic", ModelUsed: "claude-sonnet-4-6", ModelAsked: "claude-sonnet-4-6", Status: 429, RateLimited: true, RetryAfter: fp(37)},
		{CreatedAt: now.Add(-1 * time.Hour), Provider: "zai", ModelUsed: "glm-5.2", ModelAsked: "glm-5.2", Status: 200, Success: true, In: ip(60), Out: ip(12)},
		{CreatedAt: now.Add(-30 * time.Minute), Provider: "anthropic", Probe: true, Status: 200, ModelUsed: "claude-sonnet-4-6"},
	} {
		if _, err := db.RecordUsageEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{db: db}

	// default: newest first, probes included, limit applies
	m := getJSON(t, s, "/api/usage/events")
	evs := m["events"].([]interface{})
	if len(evs) != 4 {
		t.Fatalf("events = %d, want 4", len(evs))
	}
	first := evs[0].(map[string]interface{})
	if first["provider"] != "anthropic" || first["probe"] != true {
		t.Errorf("newest event should be the probe, got %v", first)
	}
	if _, ok := first["in_tokens"]; !ok {
		t.Errorf("event JSON missing in_tokens key")
	}

	// provider filter
	m = getJSON(t, s, "/api/usage/events?provider=zai")
	if evs := m["events"].([]interface{}); len(evs) != 1 || evs[0].(map[string]interface{})["provider"] != "zai" {
		t.Errorf("provider filter: got %v", m["events"])
	}

	// model filter
	m = getJSON(t, s, "/api/usage/events?model=glm-5.2")
	if evs := m["events"].([]interface{}); len(evs) != 1 {
		t.Errorf("model filter: got %d events", len(evs))
	}

	// rate-limited only
	m = getJSON(t, s, "/api/usage/events?rate_limited=1")
	if evs := m["events"].([]interface{}); len(evs) != 1 || evs[0].(map[string]interface{})["status"].(float64) != 429 {
		t.Errorf("rate_limited filter: got %v", m["events"])
	}

	// probes excluded
	m = getJSON(t, s, "/api/usage/events?probes=0")
	if evs := m["events"].([]interface{}); len(evs) != 3 {
		t.Errorf("probes=0: got %d events, want 3", len(evs))
	}

	// limit
	m = getJSON(t, s, "/api/usage/events?limit=2")
	if evs := m["events"].([]interface{}); len(evs) != 2 {
		t.Errorf("limit=2: got %d events", len(evs))
	}

	// since/until (RFC3339) bracket: only the middle two fall inside
	since := now.Add(-100 * time.Minute).Format(time.RFC3339)
	until := now.Add(-45 * time.Minute).Format(time.RFC3339)
	m = getJSON(t, s, "/api/usage/events?since="+since+"&until="+until)
	if evs := m["events"].([]interface{}); len(evs) != 2 {
		t.Errorf("since/until: got %d events, want 2", len(evs))
	}
}

func TestUsageAPI_Windows(t *testing.T) {
	db := newUsageTestDB(t)
	now := time.Now().UTC()
	seed := []*storage.UsageEvent{
		// window A: 10h ago, closed by TIME_ELAPSED before the next event
		{CreatedAt: now.Add(-10 * time.Hour), Provider: "anthropic", ModelUsed: "claude-sonnet-4-6", Status: 200, Success: true, In: ip(50), Out: ip(5)},
		// window B: 1h ago with a provider-reported 5h utilization snapshot
		{CreatedAt: now.Add(-1 * time.Hour), Provider: "anthropic", ModelUsed: "claude-sonnet-4-6", Status: 200, Success: true, In: ip(100), Out: ip(20), CacheRead: ip(400), QuotaDimension: "5h", QuotaUtilization: fp(0.42), QuotaResetAt: tp(now.Add(4 * time.Hour))},
	}
	for _, e := range seed {
		if _, err := db.RecordUsageEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{db: db}

	m := getJSON(t, s, "/api/usage/windows?provider=anthropic")
	ws := m["windows"].([]interface{})
	if len(ws) != 2 {
		t.Fatalf("windows = %d, want 2 (got %v)", len(ws), m["windows"])
	}
	open := ws[0].(map[string]interface{}) // most recent first
	if open["ended_at"] != nil {
		t.Errorf("current window should be open, got ended_at %v", open["ended_at"])
	}
	if open["in_tokens"].(float64) != 100 || open["cache_read_tokens"].(float64) != 400 {
		t.Errorf("window totals wrong: %v", open)
	}
	if open["requests"].(float64) != 1 {
		t.Errorf("window requests = %v, want 1", open["requests"])
	}
	quota, ok := open["quota"].(map[string]interface{})
	if !ok || quota["utilization"].(float64) != 0.42 {
		t.Errorf("window quota = %v, want utilization 0.42", open["quota"])
	}
	closed := ws[1].(map[string]interface{})
	// oldest window: its TIME_ELAPSED end is start-relative, and the true start
	// predates recorded history → downgraded to UNKNOWN by design
	if closed["end_reason"] != storage.EndUnknown {
		t.Errorf("oldest window end_reason = %v, want %q", closed["end_reason"], storage.EndUnknown)
	}

	// limit=1 keeps only the current window
	m = getJSON(t, s, "/api/usage/windows?provider=anthropic&limit=1")
	if ws := m["windows"].([]interface{}); len(ws) != 1 {
		t.Errorf("limit=1: got %d windows", len(ws))
	}

	// no provider param: windows for every provider with events (only anthropic here)
	m = getJSON(t, s, "/api/usage/windows")
	if ws := m["windows"].([]interface{}); len(ws) != 2 {
		t.Errorf("all-providers: got %d windows, want 2", len(ws))
	}
}

func TestUsageAPI_Totals(t *testing.T) {
	db := newUsageTestDB(t)
	now := time.Now().UTC()
	for _, e := range []*storage.UsageEvent{
		{CreatedAt: now.Add(-1 * time.Hour), Provider: "anthropic", ModelUsed: "claude-sonnet-4-6", Status: 200, Success: true, In: ip(100), Out: ip(20)},
		{CreatedAt: now.Add(-30 * time.Minute), Provider: "zai", ModelUsed: "glm-5.2", Status: 200, Success: true, In: ip(60), Out: ip(12), Reasoning: ip(12)},
		{CreatedAt: now.Add(-10 * time.Minute), Provider: "anthropic", ModelUsed: "claude-sonnet-4-6", Status: 200, Success: true, In: ip(10), Out: ip(2), UsagePartial: true},
	} {
		if _, err := db.RecordUsageEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{db: db}

	m := getJSON(t, s, "/api/usage/totals")
	if m["by"] != "provider" || m["period"] != "all" {
		t.Errorf("defaults: by=%v period=%v", m["by"], m["period"])
	}
	totals := m["totals"].([]interface{})
	if len(totals) != 2 {
		t.Fatalf("totals = %d groups, want 2", len(totals))
	}
	anthropic := totals[0].(map[string]interface{}) // ordered by group name
	if anthropic["group"] != "anthropic" || anthropic["requests"].(float64) != 2 || anthropic["in_tokens"].(float64) != 110 || anthropic["partial_events"].(float64) != 1 {
		t.Errorf("anthropic totals: %v", anthropic)
	}

	m = getJSON(t, s, "/api/usage/totals?by=model&period=today")
	if m["by"] != "model" {
		t.Errorf("by=model: got %v", m["by"])
	}
	if totals := m["totals"].([]interface{}); len(totals) != 2 {
		t.Errorf("by=model: %d groups, want 2", len(totals))
	}
}

func TestUsageAPI_Quota(t *testing.T) {
	db := newUsageTestDB(t)
	now := time.Now().UTC()
	for _, e := range []*storage.UsageEvent{
		{CreatedAt: now.Add(-2 * time.Hour), Provider: "anthropic", Status: 200, QuotaDimension: "5h", QuotaUtilization: fp(0.30)},
		{CreatedAt: now.Add(-1 * time.Hour), Provider: "anthropic", Status: 200, QuotaDimension: "5h", QuotaUtilization: fp(0.55), QuotaResetAt: tp(now.Add(4 * time.Hour))},
		{CreatedAt: now.Add(-1 * time.Hour), Provider: "anthropic", Status: 200, QuotaDimension: "7d", QuotaUtilization: fp(0.12)},
		{CreatedAt: now.Add(-30 * time.Minute), Provider: "zai", Status: 200}, // no quota reported
	} {
		if _, err := db.RecordUsageEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{db: db}

	m := getJSON(t, s, "/api/usage/quota")
	qs := m["quota"].([]interface{})
	if len(qs) != 2 {
		t.Fatalf("quota = %d snapshots, want 2 (one per provider+dimension)", len(qs))
	}
	five := qs[0].(map[string]interface{}) // ordered by provider, dimension
	if five["dimension"] != "5h" || five["utilization"].(float64) != 0.55 {
		t.Errorf("5h snapshot: %v", five)
	}
	if five["reset_at"] == nil {
		t.Errorf("5h snapshot should carry reset_at")
	}
	seven := qs[1].(map[string]interface{})
	if seven["dimension"] != "7d" || seven["utilization"].(float64) != 0.12 {
		t.Errorf("7d snapshot: %v", seven)
	}
}

// Storage failures must be loud: a broken DB surfaces as HTTP 500, never as
// HTTP 200 with an empty payload (the failure mode that once hid the
// COALESCE scan bug as "no usage").
func TestUsageAPI_StorageErrorsAre500(t *testing.T) {
	db := newUsageTestDB(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s := &Server{db: db}
	for _, p := range []string{"/api/usage/windows", "/api/usage/events", "/api/usage/totals", "/api/usage/quota"} {
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s = %d, want 500 (storage error must not masquerade as empty data)", p, rec.Code)
		}
	}
}
