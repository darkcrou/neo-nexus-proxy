package proxy

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lynuxis2026-pixel/nexus-proxy/internal/storage"
)

func TestProviderOverrideHeader(t *testing.T) {
	var aHits, bHits int32
	a := countingOAIServer("from-a", &aHits)
	b := countingOAIServer("from-b", &bHits)
	defer a.Close()
	defer b.Close()

	h := buildTestHandler(t, []testProv{{"a", "free", a.URL}, {"b", "premium", b.URL}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude-haiku-4-5","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Nexus-Provider", "b") // pin to b (would normally route to free 'a')
	h.HandleMessages(rec, req)

	if bHits != 1 || aHits != 0 {
		t.Fatalf("a=%d b=%d; the pinned provider 'b' should be used exclusively", aHits, bHits)
	}
	if got := rec.Header().Get("X-Nexus-Provider"); got != "b" {
		t.Errorf("served by %q, want b", got)
	}
}

func TestProviderOverrideUnknownProvider(t *testing.T) {
	srv := openAIServer(http.StatusOK, "x")
	defer srv.Close()
	h := buildTestHandler(t, []testProv{{"a", "free", srv.URL}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Nexus-Provider", "doesnotexist")
	h.HandleMessages(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("pinning an unknown provider should 400, got %d", rec.Code)
	}
}

func TestTierHeaderPinsTier(t *testing.T) {
	var aHits, bHits int32
	a := countingOAIServer("a", &aHits)
	b := countingOAIServer("b", &bHits)
	defer a.Close()
	defer b.Close()
	h := buildTestHandler(t, []testProv{{"a", "free", a.URL}, {"b", "premium", b.URL}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude-haiku-4-5","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Nexus-Tier", "premium") // an agent harness pinning this skill to premium
	h.HandleMessages(rec, req)

	if bHits != 1 || aHits != 0 {
		t.Fatalf("X-Nexus-Tier: premium should route to the premium provider; a=%d b=%d", aHits, bHits)
	}
}

func TestInspectCapturesPromptResponse(t *testing.T) {
	srv := openAIServer(http.StatusOK, "captured answer")
	defer srv.Close()

	h := buildTestHandler(t, []testProv{{"p", "free", srv.URL}})
	db, err := storage.New(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h.db = db
	h.inspect = true

	doMessages(h, `{"model":"claude-haiku-4-5","max_tokens":50,"messages":[{"role":"user","content":"secret-prompt-xyz"}]}`)

	recents, _ := db.GetRecentRequests(1)
	if len(recents) == 0 {
		t.Fatal("request was not logged")
	}
	d, err := db.GetRequestDetail(recents[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Prompt, "secret-prompt-xyz") {
		t.Errorf("prompt not captured: %q", d.Prompt)
	}
	if !strings.Contains(d.Response, "captured answer") {
		t.Errorf("response not captured: %q", d.Response)
	}
}

func TestInspectOmitsBase64ImagePayload(t *testing.T) {
	srv := openAIServer(http.StatusOK, "saw it")
	defer srv.Close()

	h := buildTestHandler(t, []testProv{{"p", "free", srv.URL}})
	db, err := storage.New(filepath.Join(t.TempDir(), "img.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h.db = db
	h.inspect = true

	b64 := strings.Repeat("QUJD", 200) // 800 base64 chars
	doMessages(h, `{"model":"claude-haiku-4-5","max_tokens":50,"messages":[{"role":"user","content":[`+
		`{"type":"text","text":"describe-this-picture"},`+
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"`+b64+`"}}]}]}`)

	recents, _ := db.GetRecentRequests(1)
	if len(recents) == 0 {
		t.Fatal("request was not logged")
	}
	d, err := db.GetRequestDetail(recents[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(d.Prompt, "QUJDQUJD") {
		t.Errorf("stored prompt still contains base64 image data: %.120q", d.Prompt)
	}
	if !strings.Contains(d.Prompt, "[image data omitted: 800 bytes]") {
		t.Errorf("stored prompt lacks the omission marker: %.300q", d.Prompt)
	}
	if !strings.Contains(d.Prompt, "describe-this-picture") {
		t.Errorf("stored prompt lost its text: %.300q", d.Prompt)
	}
}
