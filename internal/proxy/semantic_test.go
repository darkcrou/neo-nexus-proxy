package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEmbedCosine(t *testing.T) {
	a := embed("how do I reverse a list in python")
	same := embed("How do I reverse a list in Python?") // only case/punctuation differ
	if c := cosine(a, same); c < 0.999 {
		t.Errorf("case/punctuation-only difference should be ~1.0, got %v", c)
	}
	similar := embed("how do I reverse a list in python quickly")
	if c := cosine(a, similar); c < 0.7 {
		t.Errorf("near-identical prompt should score high, got %v", c)
	}
	diff := embed("what is the capital of france")
	if c := cosine(a, diff); c > 0.5 {
		t.Errorf("unrelated prompt should score low, got %v", c)
	}
}

func TestCacheKeyIgnoresVolatileFields(t *testing.T) {
	plain := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`)
	withMeta := []byte(`{"model":"claude-sonnet-4-6","metadata":{"user_id":"u1"},"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`)
	// content shapes differ (string vs block), so these won't be equal; assert the
	// cache_control + metadata stripping at least collapses an otherwise-equal pair:
	a := []byte(`{"model":"m","system":[{"type":"text","text":"S","cache_control":{"type":"ephemeral"}}]}`)
	b := []byte(`{"model":"m","system":[{"type":"text","text":"S"}],"metadata":{"x":1}}`)
	if cacheKey("m", a) != cacheKey("m", b) {
		t.Errorf("cache_control + metadata must not affect the cache key")
	}
	_ = plain
	_ = withMeta
}

// semanticMock returns a chat.completion and counts upstream calls.
func semanticMock(calls *int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id": "x", "object": "chat.completion", "model": "m",
			"choices": []interface{}{map[string]interface{}{"index": 0, "finish_reason": "stop",
				"message": map[string]interface{}{"role": "assistant", "content": "answer"}}},
			"usage": map[string]interface{}{"prompt_tokens": 5, "completion_tokens": 2},
		})
	}))
}

func TestSemanticCacheHit(t *testing.T) {
	var calls int
	srv := semanticMock(&calls)
	defer srv.Close()

	h := buildTestHandler(t, []testProv{{"p", "free", srv.URL}})
	h.cache = newResponseCache(time.Minute, 100, true, 0.7)

	first := `{"model":"gpt-4o","messages":[{"role":"user","content":"how do I reverse a list in python"}]}`
	reworded := `{"model":"gpt-4o","messages":[{"role":"user","content":"How do I reverse a list in Python quickly?"}]}`

	rec1 := chatCompletions(h, first)
	rec2 := chatCompletions(h, reworded)

	if calls != 1 {
		t.Errorf("near-identical prompt must be served from the semantic cache; upstream calls=%d (want 1)", calls)
	}
	if rec1.Header().Get("X-Nexus-Cache") == "HIT" {
		t.Error("first request should be a MISS")
	}
	if rec2.Header().Get("X-Nexus-Cache") != "HIT" {
		t.Errorf("reworded request should be a semantic HIT, got %q", rec2.Header().Get("X-Nexus-Cache"))
	}
}

func TestSemanticCacheSkipsToolRequests(t *testing.T) {
	var calls int
	srv := semanticMock(&calls)
	defer srv.Close()

	h := buildTestHandler(t, []testProv{{"p", "free", srv.URL}})
	h.cache = newResponseCache(time.Minute, 100, true, 0.5)

	tools := `"tools":[{"function":{"name":"x","parameters":{}},"type":"function"}]`
	a := `{"model":"gpt-4o",` + tools + `,"messages":[{"role":"user","content":"reverse a list in python"}]}`
	b := `{"model":"gpt-4o",` + tools + `,"messages":[{"role":"user","content":"reverse a list in python now"}]}`

	chatCompletions(h, a)
	chatCompletions(h, b)

	if calls != 2 {
		t.Errorf("tool requests must never be served as a semantic match; upstream calls=%d (want 2)", calls)
	}
}

// TestSemanticCacheSkipsImageRequests guards against the bug where the
// semantic cache keyed purely on prompt text, so two requests with identical
// text but different attached images could collide and serve the wrong
// cached answer. Both requests below have identical text but different
// base64 image data — they must never produce a semantic-cache hit against
// each other.
func TestSemanticCacheSkipsImageRequests(t *testing.T) {
	var calls int
	srv := semanticMock(&calls)
	defer srv.Close()

	h := buildTestHandler(t, []testProv{{"p", "free", srv.URL}})
	h.cache = newResponseCache(time.Minute, 100, true, 0.5)

	imgBlock := func(data string) string {
		return `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + data + `"}}`
	}
	a := `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":[` +
		imgBlock("aGVsbG8xMjM0NTY3ODkwYWJjZGVmZ2g=") + `,{"type":"text","text":"what is in this image?"}]}]}`
	b := `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":[` +
		imgBlock("d29ybGQ5ODc2NTQzMjEwemyeHd2dXQ=") + `,{"type":"text","text":"what is in this image?"}]}]}`

	rec1 := doMessages(h, a)
	rec2 := doMessages(h, b)

	if calls != 2 {
		t.Errorf("image-bearing requests must never be served as a semantic match against each other; upstream calls=%d (want 2)", calls)
	}
	if rec1.Header().Get("X-Nexus-Cache") == "HIT" {
		t.Error("first image request should be a MISS")
	}
	if rec2.Header().Get("X-Nexus-Cache") == "HIT" {
		t.Error("second image request (different image, same text) must not be a semantic HIT")
	}
}

// TestSemanticCacheHitViaHandleMessages is a regression check: pure-text
// semantic caching through the Anthropic-native HandleMessages path (the
// path the image-keying fix touches) must still work exactly as before.
func TestSemanticCacheHitViaHandleMessages(t *testing.T) {
	var calls int
	srv := semanticMock(&calls)
	defer srv.Close()

	h := buildTestHandler(t, []testProv{{"p", "free", srv.URL}})
	h.cache = newResponseCache(time.Minute, 100, true, 0.7)

	first := `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"how do I reverse a list in python"}]}`
	reworded := `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"How do I reverse a list in Python quickly?"}]}`

	rec1 := doMessages(h, first)
	rec2 := doMessages(h, reworded)

	if calls != 1 {
		t.Errorf("near-identical text-only prompt must still be served from the semantic cache; upstream calls=%d (want 1)", calls)
	}
	if rec1.Header().Get("X-Nexus-Cache") == "HIT" {
		t.Error("first request should be a MISS")
	}
	if rec2.Header().Get("X-Nexus-Cache") != "HIT" {
		t.Errorf("reworded text-only request should be a semantic HIT, got %q", rec2.Header().Get("X-Nexus-Cache"))
	}
}
