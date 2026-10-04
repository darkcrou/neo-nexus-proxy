package proxy

// End-to-end image matrix. The bugs this pins were all interaction bugs between
// layers (request parser x response cache x privacy firewall x provider type x
// streaming x failover), so each case drives the real handler entry points
// (HandleChatCompletions / HandleMessages) against in-process upstream servers
// and asserts on what the upstream actually received.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lynuxis2026-pixel/nexus-proxy/internal/providers"
	"github.com/lynuxis2026-pixel/nexus-proxy/internal/router"
)

const (
	e2eDataURI = "data:image/png;base64,iVBORw0KGgo="
	e2eB64     = "iVBORw0KGgo="
)

// imgUpstream is an OpenAI-compatible upstream that records every raw request
// body it receives. status != 200 makes it fail every request; sse makes a 200
// reply an SSE stream instead of JSON.
type imgUpstream struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []string
}

func newImgUpstream(t *testing.T, status int, sse bool) *imgUpstream {
	t.Helper()
	u := &imgUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.bodies = append(u.bodies, string(b))
		u.mu.Unlock()
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"message":"err"}}`))
			return
		}
		if sse {
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"a cat"},"index":0}]}`+"\n\n")
			fl.Flush()
			fmt.Fprint(w, `data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":4}}`+"\n\n")
			fl.Flush()
			fmt.Fprint(w, "data: [DONE]\n\n")
			fl.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id": "x", "object": "chat.completion", "model": "m",
			"choices": []interface{}{map[string]interface{}{"index": 0, "finish_reason": "stop",
				"message": map[string]interface{}{"role": "assistant", "content": "a cat"}}},
			"usage": map[string]interface{}{"prompt_tokens": 9, "completion_tokens": 4},
		})
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *imgUpstream) calls() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.bodies)
}

// last returns the most recent raw body (fails the test if none arrived).
func (u *imgUpstream) last(t *testing.T) string {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.bodies) == 0 {
		t.Fatal("upstream received no request")
	}
	return u.bodies[len(u.bodies)-1]
}

// imageURLParts walks a decoded request body and returns every
// {"type":"image_url"} content part found anywhere in it.
func imageURLParts(v interface{}) []map[string]interface{} {
	var out []map[string]interface{}
	switch t := v.(type) {
	case map[string]interface{}:
		if t["type"] == "image_url" {
			out = append(out, t)
		}
		for _, c := range t {
			out = append(out, imageURLParts(c)...)
		}
	case []interface{}:
		for _, c := range t {
			out = append(out, imageURLParts(c)...)
		}
	}
	return out
}

// upstreamImageURLs returns the image_url.url strings an upstream body carried.
func upstreamImageURLs(t *testing.T, raw string) []string {
	t.Helper()
	var v interface{}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("upstream body is not JSON: %v\n%s", err, raw)
	}
	var urls []string
	for _, p := range imageURLParts(v) {
		if iu, ok := p["image_url"].(map[string]interface{}); ok {
			if s, ok := iu["url"].(string); ok {
				urls = append(urls, s)
			}
		}
	}
	return urls
}

func e2eGatewayBody(extra, dataURI string) string {
	return `{"model":"gpt-4o",` + extra + `"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"what is this?"},` +
		`{"type":"image_url","image_url":{"url":"` + dataURI + `","detail":"high"}}]}]}`
}

func e2eMessagesBody(b64 string) string {
	return `{"model":"claude-sonnet-4-6","max_tokens":50,"messages":[{"role":"user","content":[` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b64 + `"}},` +
		`{"type":"text","text":"what is this?"}]}]}`
}

// 1. Gateway -> OpenAI-compatible, non-stream.
func TestImageE2E_GatewayToOpenAICompatible_NonStream(t *testing.T) {
	up := newImgUpstream(t, http.StatusOK, false)
	h := buildTestHandler(t, []testProv{{"p", "free", up.srv.URL}})

	rec := chatCompletions(h, e2eGatewayBody("", e2eDataURI))

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "a cat") {
		t.Errorf("client did not get the upstream answer: %s", rec.Body.String())
	}
	raw := up.last(t)
	if !strings.Contains(raw, e2eDataURI) {
		t.Errorf("data URI not forwarded verbatim: %s", raw)
	}
	var body map[string]interface{}
	_ = json.Unmarshal([]byte(raw), &body)
	parts := imageURLParts(body)
	if len(parts) != 1 {
		t.Fatalf("image_url parts upstream = %d, want 1: %s", len(parts), raw)
	}
	got, _ := json.Marshal(parts[0])
	want := `{"image_url":{"detail":"high","url":"` + e2eDataURI + `"},"type":"image_url"}`
	if string(got) != want {
		t.Errorf("image part = %s, want %s", got, want)
	}
}

// 2. Gateway -> OpenAI-compatible, "stream":true.
func TestImageE2E_GatewayToOpenAICompatible_Stream(t *testing.T) {
	up := newImgUpstream(t, http.StatusOK, true)
	h := buildTestHandler(t, []testProv{{"p", "free", up.srv.URL}})

	rec := chatCompletions(h, e2eGatewayBody(`"stream":true,`, e2eDataURI))

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"content":"a cat"`) || !strings.Contains(out, "[DONE]") {
		t.Errorf("stream not relayed:\n%s", out)
	}
	raw := up.last(t)
	var body map[string]interface{}
	_ = json.Unmarshal([]byte(raw), &body)
	if body["stream"] != true {
		t.Errorf("upstream stream flag = %v, want true", body["stream"])
	}
	parts := imageURLParts(body)
	if len(parts) != 1 {
		t.Fatalf("image_url parts upstream = %d, want 1: %s", len(parts), raw)
	}
	iu := parts[0]["image_url"].(map[string]interface{})
	if iu["url"] != e2eDataURI || iu["detail"] != "high" {
		t.Errorf("image_url = %+v, want url=%s detail=high", iu, e2eDataURI)
	}
}

// 3. Gateway -> Anthropic-format provider.
func TestImageE2E_GatewayToAnthropicFormat(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "x")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "y")
	var mu sync.Mutex
	var gotBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		gotBody = m
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(anthropicResponseJSON("a cat")))
	}))
	defer srv.Close()
	impl, err := providers.New(providers.Spec{Type: "bedrock", Name: "bedrock", Region: "us-east-1", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	h := anthropicNativeHandler(impl, "")

	rec := chatCompletions(h, e2eGatewayBody("", e2eDataURI))

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	msgs, _ := gotBody["messages"].([]interface{})
	if len(msgs) != 1 {
		t.Fatalf("upstream messages = %+v", gotBody["messages"])
	}
	blocks, _ := msgs[0].(map[string]interface{})["content"].([]interface{})
	if len(blocks) != 2 {
		t.Fatalf("content blocks = %+v, want text + image", blocks)
	}
	var images int
	for _, b := range blocks {
		bm := b.(map[string]interface{})
		switch bm["type"] {
		case "text":
			if bm["text"] == "" {
				t.Errorf("empty text block present: %+v", bm)
			}
		case "image":
			images++
			src, _ := bm["source"].(map[string]interface{})
			if src["type"] != "base64" || src["media_type"] != "image/png" || src["data"] != e2eB64 {
				t.Errorf("image block = %+v, want base64 image/png %s", bm, e2eB64)
			}
		default:
			t.Errorf("unexpected block %+v", bm)
		}
	}
	if images != 1 {
		t.Errorf("image blocks = %d, want 1", images)
	}
}

// 4. /v1/messages -> OpenAI-compatible, image only inside a tool_result; the
// vision_model override must apply and the upstream must see an image_url part.
func TestImageE2E_MessagesToolResultImage_VisionOverride(t *testing.T) {
	up := newImgUpstream(t, http.StatusOK, false)
	impl, err := providers.New(providers.Spec{
		Name: "visionprov", Type: "openai-compatible", BaseURL: up.srv.URL,
		Models: []string{"llama-x"}, VisionModel: "llama-vision-90b",
	})
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(router.StrategyAuto)
	rt.AddProvider(&router.Provider{Name: "visionprov", Tier: "free", Healthy: true})
	h := &Handler{
		httpClient: &http.Client{Timeout: 10 * time.Second},
		router:     rt,
		providers:  map[string]*activeProvider{"visionprov": {impl: impl, apiKey: "k"}},
	}

	body := `{"model":"claude-sonnet-4-6","max_tokens":50,"messages":[` +
		`{"role":"user","content":"look at the file"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"a.png"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + e2eB64 + `"}}]}]}]}`
	rec := doMessages(h, body)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	raw := up.last(t)
	var m map[string]interface{}
	_ = json.Unmarshal([]byte(raw), &m)
	if m["model"] != "llama-vision-90b" {
		t.Errorf("upstream model = %v, want vision_model override llama-vision-90b", m["model"])
	}
	urls := upstreamImageURLs(t, raw)
	if len(urls) != 1 || urls[0] != e2eDataURI {
		t.Errorf("upstream image_url urls = %v, want [%s]\n%s", urls, e2eDataURI, raw)
	}
}

// 5a. Failover on /v1/chat/completions: first provider 500, second gets the image intact.
func TestImageE2E_GatewayFailover(t *testing.T) {
	bad := newImgUpstream(t, http.StatusInternalServerError, false)
	good := newImgUpstream(t, http.StatusOK, false)
	h := buildTestHandler(t, []testProv{{"bad", "free", bad.srv.URL}, {"good", "free", good.srv.URL}})

	rec := chatCompletions(h, e2eGatewayBody("", e2eDataURI))

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Nexus-Provider"); got != "good" {
		t.Fatalf("X-Nexus-Provider = %q, want good", got)
	}
	if bad.calls() == 0 {
		t.Fatal("first provider was never tried")
	}
	for name, up := range map[string]*imgUpstream{"bad": bad, "good": good} {
		urls := upstreamImageURLs(t, up.last(t))
		if len(urls) != 1 || urls[0] != e2eDataURI {
			t.Errorf("%s upstream image urls = %v, want [%s]", name, urls, e2eDataURI)
		}
	}
}

// 5b. Failover on /v1/messages: first provider 500, second gets the image intact.
func TestImageE2E_MessagesFailover(t *testing.T) {
	bad := newImgUpstream(t, http.StatusInternalServerError, false)
	good := newImgUpstream(t, http.StatusOK, false)
	h := buildTestHandler(t, []testProv{{"bad", "free", bad.srv.URL}, {"good", "free", good.srv.URL}})

	rec := doMessages(h, e2eMessagesBody(e2eB64))

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Nexus-Provider"); got != "good" {
		t.Fatalf("X-Nexus-Provider = %q, want good", got)
	}
	if bad.calls() == 0 {
		t.Fatal("first provider was never tried")
	}
	for name, up := range map[string]*imgUpstream{"bad": bad, "good": good} {
		urls := upstreamImageURLs(t, up.last(t))
		if len(urls) != 1 || urls[0] != e2eDataURI {
			t.Errorf("%s upstream image urls = %v, want [%s]", name, urls, e2eDataURI)
		}
	}
}

// 5c. Failover across provider types: an OpenAI-compatible provider fails and
// the Anthropic-format provider behind it must receive a proper base64 block.
func TestImageE2E_GatewayFailoverOpenAIToAnthropicFormat(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "x")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "y")
	bad := newImgUpstream(t, http.StatusInternalServerError, false)
	var mu sync.Mutex
	var gotRaw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotRaw = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(anthropicResponseJSON("a cat")))
	}))
	defer srv.Close()

	oai, err := providers.New(providers.Spec{Name: "bad", Type: "openai-compatible", BaseURL: bad.srv.URL, Tier: "free"})
	if err != nil {
		t.Fatal(err)
	}
	bed, err := providers.New(providers.Spec{Type: "bedrock", Name: "bedrock", Region: "us-east-1", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(router.StrategyAuto)
	rt.AddProvider(&router.Provider{Name: "bad", Tier: "free", Healthy: true})
	rt.AddProvider(&router.Provider{Name: "bedrock", Tier: "free", Healthy: true})
	h := &Handler{
		httpClient: &http.Client{Timeout: 10 * time.Second},
		router:     rt,
		providers: map[string]*activeProvider{
			"bad":     {impl: oai, apiKey: "k"},
			"bedrock": {impl: bed, apiKey: ""},
		},
	}

	rec := chatCompletions(h, e2eGatewayBody("", e2eDataURI))

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if bad.calls() == 0 {
		t.Fatal("OpenAI-compatible provider was never tried first")
	}
	mu.Lock()
	defer mu.Unlock()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(gotRaw), &m); err != nil {
		t.Fatalf("anthropic upstream body not JSON: %v\n%s", err, gotRaw)
	}
	if !strings.Contains(gotRaw, `"data":"`+e2eB64+`"`) || !strings.Contains(gotRaw, `"media_type":"image/png"`) {
		t.Errorf("anthropic-format upstream did not get a base64 image block: %s", gotRaw)
	}
	if len(imageURLParts(m)) != 0 {
		t.Errorf("OpenAI image_url part leaked into Anthropic-format body: %s", gotRaw)
	}
}

// 6. Exact response cache (semantic off): identical image request twice -> HIT
// with one upstream call; same text but a different image -> separate upstream calls.
func TestImageE2E_ExactCache(t *testing.T) {
	const otherB64 = "R0lGODlhAQABAAAAACw="
	cases := []struct {
		name string
		do   func(h *Handler, b64 string) *httptest.ResponseRecorder
	}{
		{"gateway", func(h *Handler, b64 string) *httptest.ResponseRecorder {
			return chatCompletions(h, e2eGatewayBody("", "data:image/png;base64,"+b64))
		}},
		{"messages", func(h *Handler, b64 string) *httptest.ResponseRecorder {
			return doMessages(h, e2eMessagesBody(b64))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newImgUpstream(t, http.StatusOK, false)
			h := buildTestHandler(t, []testProv{{"p", "free", up.srv.URL}})
			h.cache = newResponseCache(time.Minute, 100, false, 0)

			r1 := tc.do(h, e2eB64)
			r2 := tc.do(h, e2eB64)
			if r1.Code != 200 || r2.Code != 200 {
				t.Fatalf("status = %d / %d", r1.Code, r2.Code)
			}
			if r1.Header().Get("X-Nexus-Cache") == "HIT" {
				t.Error("first request must be a MISS")
			}
			if r2.Header().Get("X-Nexus-Cache") != "HIT" {
				t.Errorf("identical image request must be an exact-cache HIT, got %q", r2.Header().Get("X-Nexus-Cache"))
			}
			if up.calls() != 1 {
				t.Fatalf("upstream calls after identical requests = %d, want 1", up.calls())
			}

			r3 := tc.do(h, otherB64)
			if r3.Code != 200 {
				t.Fatalf("status = %d", r3.Code)
			}
			if r3.Header().Get("X-Nexus-Cache") == "HIT" {
				t.Error("same text with a different image must not be a cache HIT")
			}
			if up.calls() != 2 {
				t.Fatalf("upstream calls after different-image request = %d, want 2", up.calls())
			}
			urls := upstreamImageURLs(t, up.last(t))
			if len(urls) != 1 || urls[0] != "data:image/png;base64,"+otherB64 {
				t.Errorf("different image did not reach upstream intact: %v", urls)
			}
		})
	}
}

// 7. Semantic cache: same text, different image -> never a semantic HIT, and
// each image reaches the upstream.
func TestImageE2E_SemanticCacheNeverHitsAcrossImages(t *testing.T) {
	const otherB64 = "R0lGODlhAQABAAAAACw="
	cases := []struct {
		name string
		do   func(h *Handler, b64 string) *httptest.ResponseRecorder
	}{
		{"gateway", func(h *Handler, b64 string) *httptest.ResponseRecorder {
			return chatCompletions(h, e2eGatewayBody("", "data:image/png;base64,"+b64))
		}},
		{"messages", func(h *Handler, b64 string) *httptest.ResponseRecorder {
			return doMessages(h, e2eMessagesBody(b64))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newImgUpstream(t, http.StatusOK, false)
			h := buildTestHandler(t, []testProv{{"p", "free", up.srv.URL}})
			h.cache = newResponseCache(time.Minute, 100, true, 0.5)

			r1 := tc.do(h, e2eB64)
			r2 := tc.do(h, otherB64)
			if r1.Code != 200 || r2.Code != 200 {
				t.Fatalf("status = %d / %d", r1.Code, r2.Code)
			}
			if r1.Header().Get("X-Nexus-Cache") == "HIT" || r2.Header().Get("X-Nexus-Cache") == "HIT" {
				t.Errorf("image request served as a cache HIT (%q / %q)",
					r1.Header().Get("X-Nexus-Cache"), r2.Header().Get("X-Nexus-Cache"))
			}
			if up.calls() != 2 {
				t.Fatalf("upstream calls = %d, want 2", up.calls())
			}
			up.mu.Lock()
			defer up.mu.Unlock()
			for i, want := range []string{e2eB64, otherB64} {
				urls := upstreamImageURLs(t, up.bodies[i])
				if len(urls) != 1 || urls[0] != "data:image/png;base64,"+want {
					t.Errorf("upstream call %d image urls = %v, want data URI of %s", i, urls, want)
				}
			}
		})
	}
}

// 8. Firewall enabled with an image whose base64 happens to contain secret-shaped
// substrings: the image must reach the upstream byte-identical while a real
// secret in the text part is still masked.
func TestImageE2E_FirewallLeavesImageIntactButRedactsText(t *testing.T) {
	const secret = "sk-ant-api03ZZZZYYYYXXXXWWWWVVVV"
	uri := "data:image/png;base64," + imgNoise

	cases := []struct {
		name string
		do   func(h *Handler) *httptest.ResponseRecorder
	}{
		{"gateway", func(h *Handler) *httptest.ResponseRecorder {
			body, _ := json.Marshal(map[string]interface{}{
				"model": "gpt-4o",
				"messages": []interface{}{map[string]interface{}{
					"role": "user",
					"content": []interface{}{
						map[string]interface{}{"type": "text", "text": "deploy with " + secret},
						map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": uri, "detail": "low"}},
					},
				}},
			})
			return chatCompletions(h, string(body))
		}},
		{"messages", func(h *Handler) *httptest.ResponseRecorder {
			body, _ := json.Marshal(map[string]interface{}{
				"model": "claude-sonnet-4-6", "max_tokens": 50,
				"messages": []interface{}{map[string]interface{}{
					"role": "user",
					"content": []interface{}{
						map[string]interface{}{"type": "text", "text": "deploy with " + secret},
						map[string]interface{}{"type": "image", "source": map[string]interface{}{
							"type": "base64", "media_type": "image/png", "data": imgNoise,
						}},
					},
				}},
			})
			return doMessages(h, string(body))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newImgUpstream(t, http.StatusOK, false)
			h := buildTestHandler(t, []testProv{{"p", "free", up.srv.URL}})
			h.firewall = &redactor{}

			rec := tc.do(h)

			if rec.Code != 200 {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			raw := up.last(t)
			if strings.Contains(raw, secret) {
				t.Errorf("SECRET LEAKED to provider:\n%s", raw)
			}
			if !strings.Contains(raw, "[NX_REDACTED_APIKEY_1]") {
				t.Errorf("text secret should have been replaced by a placeholder:\n%s", raw)
			}
			urls := upstreamImageURLs(t, raw)
			if len(urls) != 1 || urls[0] != uri {
				t.Errorf("image data URI was modified by the firewall:\n got %v\nwant %s", urls, uri)
			}
		})
	}
}
