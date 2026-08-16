package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lynuxis2026-pixel/nexus-proxy/internal/providers"
	"github.com/lynuxis2026-pixel/nexus-proxy/internal/router"
)

// anthropicNativeHandler builds a Handler with a single Anthropic-native
// provider (Bedrock/Vertex) pointed at an in-process test server.
func anthropicNativeHandler(impl providers.Provider, apiKey string) *Handler {
	rt := router.New(router.StrategyAuto)
	rt.AddProvider(&router.Provider{Name: impl.Name(), Tier: "premium", Healthy: true})
	return &Handler{
		httpClient: &http.Client{Timeout: 10 * time.Second},
		router:     rt,
		providers:  map[string]*activeProvider{impl.Name(): {impl: impl, apiKey: apiKey}},
	}
}

// ─── test helpers ───────────────────────────────────────────────────────────

type testProv struct {
	name, tier, url string
}

// buildTestHandler wires a Handler with custom OpenAI-compatible providers
// pointing at in-process httptest servers — exercising the real handler path.
func buildTestHandler(t *testing.T, provs []testProv) *Handler {
	t.Helper()
	rt := router.New(router.StrategyAuto)
	active := map[string]*activeProvider{}
	for _, p := range provs {
		impl, err := providers.New(providers.Spec{Name: p.name, Type: "openai-compatible", BaseURL: p.url, Tier: p.tier})
		if err != nil {
			t.Fatalf("build provider %s: %v", p.name, err)
		}
		active[p.name] = &activeProvider{impl: impl, apiKey: "k"}
		rt.AddProvider(&router.Provider{Name: p.name, Tier: p.tier, Healthy: true})
	}
	return &Handler{
		httpClient: &http.Client{Timeout: 10 * time.Second},
		router:     rt,
		providers:  active,
	}
}

func openAIServer(status int, content string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"message":"err"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id": "x", "object": "chat.completion", "model": "m",
			"choices": []interface{}{map[string]interface{}{
				"index": 0, "finish_reason": "stop",
				"message": map[string]interface{}{"role": "assistant", "content": content},
			}},
			"usage": map[string]interface{}{"prompt_tokens": 10, "completion_tokens": 5},
		})
	}))
}

func doMessages(h *Handler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	h.HandleMessages(rec, req)
	return rec
}

// ─── tests ──────────────────────────────────────────────────────────────────

func TestHandleMessages_OpenAIProvider(t *testing.T) {
	srv := openAIServer(http.StatusOK, "Hello from provider")
	defer srv.Close()
	h := buildTestHandler(t, []testProv{{"test", "free", srv.URL}})

	rec := doMessages(h, `{"model":"claude-haiku-4-5","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("X-Nexus-Provider"); got != "test" {
		t.Errorf("X-Nexus-Provider = %q", got)
	}
	var resp AnthropicResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, rec.Body.String())
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "Hello from provider" {
		t.Errorf("content = %+v", resp.Content)
	}
	if resp.Model != "claude-haiku-4-5" {
		t.Errorf("model = %q (should echo the requested Claude model)", resp.Model)
	}
}

func TestHandleMessages_Streaming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, c := range []string{
			`{"choices":[{"delta":{"content":"Hello "},"finish_reason":null}]}`,
			`{"choices":[{"delta":{"content":"world"},"finish_reason":null}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2}}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", c)
			fl.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()
	h := buildTestHandler(t, []testProv{{"test", "free", srv.URL}})

	rec := doMessages(h, `{"model":"claude-haiku-4-5","stream":true,"max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`)

	out := rec.Body.String()
	for _, want := range []string{"event: message_start", "text_delta", "Hello ", "world", "event: message_stop"} {
		if !strings.Contains(out, want) {
			t.Errorf("stream missing %q\n--- got ---\n%s", want, out)
		}
	}
}

func TestHandleMessages_Failover(t *testing.T) {
	bad := openAIServer(http.StatusServiceUnavailable, "")
	defer bad.Close()
	good := openAIServer(http.StatusOK, "from good")
	defer good.Close()
	// Both free → chain follows add order: bad first, then good.
	h := buildTestHandler(t, []testProv{{"bad", "free", bad.URL}, {"good", "free", good.URL}})

	rec := doMessages(h, `{"model":"claude-haiku-4-5","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != 200 {
		t.Fatalf("status = %d (expected failover to good)", rec.Code)
	}
	if got := rec.Header().Get("X-Nexus-Provider"); got != "good" {
		t.Errorf("X-Nexus-Provider = %q, want good (failover)", got)
	}
	var resp AnthropicResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Content) == 0 || resp.Content[0].Text != "from good" {
		t.Errorf("content = %+v, want 'from good'", resp.Content)
	}
}

// flakyServer returns a test server whose handler returns `failStatus` for
// the first failCount requests, then 200 (via openAIServer's success body
// shape) for every request after that. hits reports the total request count.
func flakyServer(failStatus int, failCount int, content string) (*httptest.Server, *int32) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if int(n) <= failCount {
			w.WriteHeader(failStatus)
			_, _ = w.Write([]byte(`{"error":{"message":"err"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id": "x", "object": "chat.completion", "model": "m",
			"choices": []interface{}{map[string]interface{}{
				"index": 0, "finish_reason": "stop",
				"message": map[string]interface{}{"role": "assistant", "content": content},
			}},
			"usage": map[string]interface{}{"prompt_tokens": 10, "completion_tokens": 5},
		})
	}))
	return srv, &hits
}

// TestHandleMessages_RetryBeforeFailover: a provider that fails twice with a
// non-429 retryable status (503) then succeeds on the third attempt must
// serve the request itself — without the chain-walk ever advancing to the
// next provider (P4's retry-then-pass-through, tried before failover).
func TestHandleMessages_RetryBeforeFailover(t *testing.T) {
	flaky, flakyHits := flakyServer(http.StatusServiceUnavailable, 2, "from flaky")
	defer flaky.Close()
	good := openAIServer(http.StatusOK, "from good")
	defer good.Close()
	h := buildTestHandler(t, []testProv{{"flaky", "free", flaky.URL}, {"good", "free", good.URL}})

	rec := doMessages(h, `{"model":"claude-haiku-4-5","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Nexus-Provider"); got != "flaky" {
		t.Errorf("X-Nexus-Provider = %q, want flaky (retry-before-failover, should not advance to good)", got)
	}
	if got := atomic.LoadInt32(flakyHits); got != 3 {
		t.Errorf("flaky hits = %d, want 3 (2 failed attempts + 1 success)", got)
	}
	var resp AnthropicResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Content) == 0 || resp.Content[0].Text != "from flaky" {
		t.Errorf("content = %+v, want 'from flaky'", resp.Content)
	}
}

// TestHandleMessages_RetryExhaustedStillFailsOver: a provider that fails all
// 3 attempts with a non-429 retryable status must still fail over to the
// next chain entry — proves the retry loop doesn't swallow the existing
// next-provider fallback.
func TestHandleMessages_RetryExhaustedStillFailsOver(t *testing.T) {
	bad, badHits := flakyServer(http.StatusServiceUnavailable, 1<<30, "") // never recovers
	defer bad.Close()
	good := openAIServer(http.StatusOK, "from good")
	defer good.Close()
	h := buildTestHandler(t, []testProv{{"bad", "free", bad.URL}, {"good", "free", good.URL}})

	rec := doMessages(h, `{"model":"claude-haiku-4-5","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != 200 {
		t.Fatalf("status = %d (expected eventual failover to good)", rec.Code)
	}
	if got := rec.Header().Get("X-Nexus-Provider"); got != "good" {
		t.Errorf("X-Nexus-Provider = %q, want good (failover after retries exhausted)", got)
	}
	if got := atomic.LoadInt32(badHits); got != maxProviderAttempts {
		t.Errorf("bad hits = %d, want %d (all retries exhausted before failover)", got, maxProviderAttempts)
	}
}

func TestHandleMessages_BudgetForcesFree(t *testing.T) {
	prem := openAIServer(http.StatusOK, "premium-reply")
	defer prem.Close()
	free := openAIServer(http.StatusOK, "free-reply")
	defer free.Close()
	h := buildTestHandler(t, []testProv{{"prem", "premium", prem.URL}, {"free", "free", free.URL}})
	h.budget = newBudgetTracker(0.01, 1.0, "", 0) // already 1.0 spent → over the 0.01 cap

	// A complex prompt normally routes to premium first; over budget → free only.
	rec := doMessages(h, `{"model":"claude-sonnet-4-6","max_tokens":50,"messages":[{"role":"user","content":"Design the complete system architecture for a scalable platform"}]}`)

	if got := rec.Header().Get("X-Nexus-Provider"); got != "free" {
		t.Errorf("over budget should route to free, got provider %q", got)
	}
}

func TestHandleMessages_AzureProvider(t *testing.T) {
	var gotAPIKey, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("api-key")
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []interface{}{map[string]interface{}{"index": 0, "finish_reason": "stop",
				"message": map[string]interface{}{"role": "assistant", "content": "azure-reply"}}},
			"usage": map[string]interface{}{"prompt_tokens": 3, "completion_tokens": 2},
		})
	}))
	defer srv.Close()

	impl, err := providers.New(providers.Spec{Type: "azure", Name: "azure", BaseURL: srv.URL, APIKey: "secret-key", APIVersion: "2024-10-21"})
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(router.StrategyAuto)
	rt.AddProvider(&router.Provider{Name: "azure", Tier: "premium", Healthy: true})
	h := &Handler{
		httpClient: &http.Client{Timeout: 10 * time.Second},
		router:     rt,
		providers:  map[string]*activeProvider{"azure": {impl: impl, apiKey: "secret-key"}},
	}

	rec := doMessages(h, `{"model":"claude-haiku-4-5","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`)

	if gotAPIKey != "secret-key" {
		t.Errorf("Azure should authenticate via the api-key header, got %q", gotAPIKey)
	}
	if gotAuth != "" {
		t.Errorf("Azure should NOT send a Bearer Authorization header, got %q", gotAuth)
	}
	var resp AnthropicResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Content) == 0 || resp.Content[0].Text != "azure-reply" {
		t.Errorf("content = %+v, want 'azure-reply'", resp.Content)
	}
}

func TestHandleMessages_ZeroConfigBadRequest(t *testing.T) {
	h := buildTestHandler(t, nil) // no providers → zero-config (forwards to Anthropic)
	rec := doMessages(h, `not json`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON should be 400, got %d", rec.Code)
	}
}

// anthropicResponseJSON is a canned Anthropic Messages response body.
func anthropicResponseJSON(text string) string {
	return `{"id":"msg_1","type":"message","role":"assistant","model":"claude","content":[{"type":"text","text":"` +
		text + `"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":3}}`
}

func TestHandleMessages_Bedrock(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDTEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secretkey")
	var gotPath, gotAuth, gotAmzDate, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotAmzDate = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-Amz-Date")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(anthropicResponseJSON("bedrock-reply")))
	}))
	defer srv.Close()

	impl, err := providers.New(providers.Spec{Type: "bedrock", Name: "bedrock", Region: "us-east-1", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	rec := doMessages(anthropicNativeHandler(impl, ""), `{"model":"claude-haiku-4-5","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`)

	if !strings.Contains(gotPath, "/invoke") {
		t.Errorf("URL path = %q, want .../invoke", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 ") {
		t.Errorf("missing SigV4 Authorization, got %q", gotAuth)
	}
	if gotAmzDate == "" {
		t.Error("missing X-Amz-Date header")
	}
	if !strings.Contains(gotBody, "bedrock-2023-05-31") {
		t.Errorf("body missing anthropic_version: %s", gotBody)
	}
	if strings.Contains(gotBody, `"model"`) {
		t.Errorf("body should strip model field: %s", gotBody)
	}
	var resp AnthropicResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Content) == 0 || resp.Content[0].Text != "bedrock-reply" {
		t.Errorf("relayed content = %+v", resp.Content)
	}
}

func TestHandleMessages_VertexStreaming(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(anthropicResponseJSON("vertex-reply")))
	}))
	defer srv.Close()

	impl, err := providers.New(providers.Spec{Type: "vertex", Name: "vertex", Region: "us-east5", Project: "proj", BaseURL: srv.URL, APIKey: "gcp-token"})
	if err != nil {
		t.Fatal(err)
	}
	// stream:true exercises the buffered → SSE synthesis path for Anthropic-native.
	rec := doMessages(anthropicNativeHandler(impl, "gcp-token"), `{"model":"claude-sonnet-4-6","stream":true,"max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`)

	if gotAuth != "Bearer gcp-token" {
		t.Errorf("Vertex auth = %q, want 'Bearer gcp-token'", gotAuth)
	}
	if !strings.Contains(gotPath, "rawPredict") {
		t.Errorf("path = %q, want :rawPredict", gotPath)
	}
	if !strings.Contains(gotBody, "vertex-2023-10-16") {
		t.Errorf("body missing anthropic_version: %s", gotBody)
	}
	if strings.Contains(gotBody, `"stream"`) {
		t.Errorf("body should strip stream field: %s", gotBody)
	}
	out := rec.Body.String()
	for _, want := range []string{"event: message_start", "text_delta", "vertex-reply", "event: message_stop"} {
		if !strings.Contains(out, want) {
			t.Errorf("synthesized SSE missing %q\n--- got ---\n%s", want, out)
		}
	}
}
