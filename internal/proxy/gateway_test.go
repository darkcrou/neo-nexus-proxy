package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lynuxis2026-pixel/nexus-proxy/internal/providers"
	"github.com/lynuxis2026-pixel/nexus-proxy/internal/router"
)

func chatCompletions(h *Handler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	h.HandleChatCompletions(rec, req)
	return rec
}

func TestGateway_OpenAIPassthrough(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]interface{}
		json.NewDecoder(r.Body).Decode(&m)
		gotModel, _ = m["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id": "x", "object": "chat.completion", "model": gotModel,
			"choices": []interface{}{map[string]interface{}{"index": 0, "finish_reason": "stop",
				"message": map[string]interface{}{"role": "assistant", "content": "hello from gateway"}}},
			"usage": map[string]interface{}{"prompt_tokens": 9, "completion_tokens": 4},
		})
	}))
	defer srv.Close()
	impl, err := providers.New(providers.Spec{Name: "groqish", Type: "openai-compatible", BaseURL: srv.URL, Tier: "free", Models: []string{"llama-x"}})
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(router.StrategyAuto)
	rt.AddProvider(&router.Provider{Name: "groqish", Tier: "free", Healthy: true})
	h := &Handler{httpClient: &http.Client{Timeout: 10 * time.Second}, router: rt, providers: map[string]*activeProvider{"groqish": {impl: impl, apiKey: "k"}}}

	rec := chatCompletions(h, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Header().Get("X-Nexus-Provider") != "groqish" {
		t.Errorf("provider header = %q", rec.Header().Get("X-Nexus-Provider"))
	}
	if gotModel != "llama-x" {
		t.Errorf("gateway should swap to the provider's model (llama-x), upstream saw %q", gotModel)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("not OpenAI JSON: %v", err)
	}
	choices, _ := resp["choices"].([]interface{})
	if len(choices) == 0 {
		t.Fatalf("no choices in %s", rec.Body.String())
	}
	msg := choices[0].(map[string]interface{})["message"].(map[string]interface{})
	if msg["content"] != "hello from gateway" {
		t.Errorf("content = %v", msg["content"])
	}
}

// TestGateway_DirectStrategy_PassesModelThrough confirms that /v1/chat/completions
// forwards the requested model id unchanged under StrategyDirect, matching the
// /v1/messages behavior in TestHandleMessages_DirectStrategy_PassesModelThrough.
func TestGateway_DirectStrategy_PassesModelThrough(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]interface{}
		json.NewDecoder(r.Body).Decode(&m)
		gotModel, _ = m["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id": "x", "object": "chat.completion", "model": gotModel,
			"choices": []interface{}{map[string]interface{}{"index": 0, "finish_reason": "stop",
				"message": map[string]interface{}{"role": "assistant", "content": "hello from gateway"}}},
			"usage": map[string]interface{}{"prompt_tokens": 9, "completion_tokens": 4},
		})
	}))
	defer srv.Close()
	impl, err := providers.New(providers.Spec{Name: "groqish", Type: "openai-compatible", BaseURL: srv.URL, Tier: "free", Models: []string{"llama-x"}})
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(router.StrategyDirect)
	rt.AddProvider(&router.Provider{Name: "groqish", Tier: "free", Healthy: true})
	h := &Handler{
		httpClient:  &http.Client{Timeout: 10 * time.Second},
		router:      rt,
		providers:   map[string]*activeProvider{"groqish": {impl: impl, apiKey: "k"}},
		directModel: true,
	}

	rec := chatCompletions(h, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if gotModel != "gpt-4o" {
		t.Errorf("direct strategy should pass the requested model through unchanged, upstream saw %q", gotModel)
	}
}

// TestGateway_ArrayContentPassthrough reproduces the bug reported by opencode /
// pi.dev clients: they send message.content as an array of text parts
// ([{"type":"text","text":"..."}]) instead of a plain string, which used to fail
// json.Unmarshal (OpenAIMessage.Content was a bare string) and return a 400 for a
// perfectly valid OpenAI-format request.
func TestGateway_ArrayContentPassthrough(t *testing.T) {
	var gotBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id": "x", "object": "chat.completion", "model": "llama-x",
			"choices": []interface{}{map[string]interface{}{"index": 0, "finish_reason": "stop",
				"message": map[string]interface{}{"role": "assistant", "content": "ok"}}},
			"usage": map[string]interface{}{"prompt_tokens": 1, "completion_tokens": 1},
		})
	}))
	defer srv.Close()
	h := buildTestHandler(t, []testProv{{"groqish", "free", srv.URL}})

	body := `{"model":"glm-5.2","messages":[
		{"role":"system","content":"be brief"},
		{"role":"user","content":[{"type":"text","text":"part1 "},{"type":"text","text":"part2"}]}
	]}`
	rec := chatCompletions(h, body)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	msgs, _ := gotBody["messages"].([]interface{})
	if len(msgs) == 0 {
		t.Fatalf("upstream never received messages: %+v", gotBody)
	}
	// Pass-through preserves the client's original body verbatim (rawMap), so the
	// array form must reach the upstream provider unchanged.
	last := msgs[len(msgs)-1].(map[string]interface{})
	parts, _ := last["content"].([]interface{})
	if len(parts) != 2 {
		t.Fatalf("expected the original 2-part content array to pass through, got %+v", last["content"])
	}
}

// TestGateway_RejectsUnsupportedContentPart ensures an unsupported part type
// (e.g. audio) produces a clear 400 rather than a generic/cryptic parse error.
func TestGateway_RejectsUnsupportedContentPart(t *testing.T) {
	h := buildTestHandler(t, nil)
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"x","format":"wav"}}]}]}`
	rec := chatCompletions(h, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "input_audio") || !strings.Contains(rec.Body.String(), "image_url") {
		t.Errorf("error should name the unsupported type and the supported ones, got: %s", rec.Body.String())
	}
}

// TestGateway_RejectsInvalidImageURL ensures a malformed image_url part is a 400.
func TestGateway_RejectsInvalidImageURL(t *testing.T) {
	h := buildTestHandler(t, nil)
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"ftp://x/a.png"}}]}]}`
	rec := chatCompletions(h, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid url") {
		t.Errorf("error should explain the invalid url, got: %s", rec.Body.String())
	}
}

const gatewayImageBody = `{"model":"gpt-4o","messages":[{"role":"user","content":[` +
	`{"type":"text","text":"what is this?"},` +
	`{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo=","detail":"high"}}]}]}`

// TestGateway_ImageURLPassthrough: an image_url request to an OpenAI-compatible
// provider is accepted and the image part reaches the upstream byte-identical
// (data URI and detail), with the model swapped as usual.
func TestGateway_ImageURLPassthrough(t *testing.T) {
	var gotBody map[string]interface{}
	var gotRaw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotRaw = string(b)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id": "x", "object": "chat.completion", "model": "llama-x",
			"choices": []interface{}{map[string]interface{}{"index": 0, "finish_reason": "stop",
				"message": map[string]interface{}{"role": "assistant", "content": "a cat"}}},
			"usage": map[string]interface{}{"prompt_tokens": 1, "completion_tokens": 1},
		})
	}))
	defer srv.Close()
	impl, err := providers.New(providers.Spec{Name: "groqish", Type: "openai-compatible", BaseURL: srv.URL, Tier: "free", Models: []string{"llama-x"}})
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(router.StrategyAuto)
	rt.AddProvider(&router.Provider{Name: "groqish", Tier: "free", Healthy: true})
	h := &Handler{httpClient: &http.Client{Timeout: 10 * time.Second}, router: rt, providers: map[string]*activeProvider{"groqish": {impl: impl, apiKey: "k"}}}

	rec := chatCompletions(h, gatewayImageBody)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotBody["model"] != "llama-x" {
		t.Errorf("model = %v, want llama-x", gotBody["model"])
	}
	msgs, _ := gotBody["messages"].([]interface{})
	if len(msgs) != 1 {
		t.Fatalf("messages = %+v", gotBody["messages"])
	}
	parts, _ := msgs[0].(map[string]interface{})["content"].([]interface{})
	if len(parts) != 2 {
		t.Fatalf("content = %+v, want 2 parts", msgs[0])
	}
	want := `{"image_url":{"detail":"high","url":"data:image/png;base64,iVBORw0KGgo="},"type":"image_url"}`
	got, _ := json.Marshal(parts[1])
	if string(got) != want {
		t.Errorf("image part = %s, want %s", got, want)
	}
	if !strings.Contains(gotRaw, "data:image/png;base64,iVBORw0KGgo=") {
		t.Errorf("data URI not forwarded verbatim: %s", gotRaw)
	}
}

// TestGateway_ImageURLToAnthropicFormatProvider: the same request routed to an
// Anthropic-format provider is converted to an Anthropic Messages body with a
// base64 image block and no empty text blocks.
func TestGateway_ImageURLToAnthropicFormatProvider(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "x")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "y")
	var gotBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(anthropicResponseJSON("a cat")))
	}))
	defer srv.Close()
	impl, err := providers.New(providers.Spec{Type: "bedrock", Name: "bedrock", Region: "us-east-1", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	h := anthropicNativeHandler(impl, "")

	rec := chatCompletions(h, gatewayImageBody)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	msgs, _ := gotBody["messages"].([]interface{})
	if len(msgs) != 1 {
		t.Fatalf("upstream messages = %+v", gotBody["messages"])
	}
	blocks, _ := msgs[0].(map[string]interface{})["content"].([]interface{})
	if len(blocks) != 2 {
		t.Fatalf("content blocks = %+v, want text + image", blocks)
	}
	text := blocks[0].(map[string]interface{})
	if text["type"] != "text" || text["text"] != "what is this?" {
		t.Errorf("block[0] = %+v", text)
	}
	img := blocks[1].(map[string]interface{})
	src, _ := img["source"].(map[string]interface{})
	if img["type"] != "image" || src["type"] != "base64" || src["media_type"] != "image/png" || src["data"] != "iVBORw0KGgo=" {
		t.Errorf("block[1] = %+v, want a base64 image block", img)
	}
	for _, b := range blocks {
		bm := b.(map[string]interface{})
		if bm["type"] == "text" && bm["text"] == "" {
			t.Errorf("empty text block present: %+v", bm)
		}
	}
}

// TestTransformOpenAIToAnthropic_PreservesContentArray asserts the "do not
// flatten" contract: when a client sends content as an array of text parts,
// converting to the Anthropic-format request must forward the same array of
// blocks (Anthropic accepts that shape natively), not a joined string.
func TestTransformOpenAIToAnthropic_PreservesContentArray(t *testing.T) {
	o := OpenAIRequest{
		Model: "gpt-4o",
		Messages: []OpenAIMessage{
			{Role: "user", Content: mustArrayContent(t, `[{"type":"text","text":"part1 "},{"type":"text","text":"part2"}]`)},
		},
	}
	a := TransformOpenAIToAnthropic(o)
	blocks, ok := a.Messages[0].Content.([]map[string]interface{})
	if !ok {
		t.Fatalf("expected content to stay an array of blocks, got %T: %+v", a.Messages[0].Content, a.Messages[0].Content)
	}
	if len(blocks) != 2 || blocks[0]["text"] != "part1 " || blocks[1]["text"] != "part2" {
		t.Errorf("blocks = %+v", blocks)
	}
}

func mustArrayContent(t *testing.T, jsonArray string) OpenAIContent {
	t.Helper()
	var c OpenAIContent
	if err := json.Unmarshal([]byte(jsonArray), &c); err != nil {
		t.Fatalf("unmarshal content: %v", err)
	}
	return c
}

func TestGateway_StreamPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"hi"},"index":0}]}`+"\n\n")
		fl.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()
	h := buildTestHandler(t, []testProv{{"groqish", "free", srv.URL}})

	rec := chatCompletions(h, `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	out := rec.Body.String()
	if !strings.Contains(out, `"content":"hi"`) || !strings.Contains(out, "[DONE]") {
		t.Errorf("stream not passed through:\n%s", out)
	}
}

func TestGateway_AnthropicProviderToOpenAI(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "x")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "y")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(anthropicResponseJSON("claude-says-hi")))
	}))
	defer srv.Close()
	impl, err := providers.New(providers.Spec{Type: "bedrock", Name: "bedrock", Region: "us-east-1", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	h := anthropicNativeHandler(impl, "")

	rec := chatCompletions(h, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)

	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("not OpenAI JSON: %v\n%s", err, rec.Body.String())
	}
	if resp["object"] != "chat.completion" {
		t.Errorf("object = %v, want chat.completion", resp["object"])
	}
	choices := resp["choices"].([]interface{})
	msg := choices[0].(map[string]interface{})["message"].(map[string]interface{})
	if msg["content"] != "claude-says-hi" {
		t.Errorf("converted content = %v", msg["content"])
	}
}

func TestGateway_Models(t *testing.T) {
	h := buildTestHandler(t, nil)
	rec := httptest.NewRecorder()
	h.HandleModels(rec, httptest.NewRequest("GET", "/v1/models", nil))
	var resp map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["object"] != "list" {
		t.Errorf("object = %v, want list", resp["object"])
	}
	if data, _ := resp["data"].([]interface{}); len(data) == 0 {
		t.Error("models list is empty")
	}
}

func TestTransformOpenAIToAnthropic(t *testing.T) {
	o := OpenAIRequest{
		Model: "gpt-4o",
		Messages: []OpenAIMessage{
			{Role: "system", Content: textContent("be brief")},
			{Role: "user", Content: textContent("hi")},
		},
	}
	a := TransformOpenAIToAnthropic(o)
	if a.System != "be brief" {
		t.Errorf("system = %v", a.System)
	}
	if len(a.Messages) != 1 || a.Messages[0].Role != "user" {
		t.Errorf("messages = %+v (system should be lifted out)", a.Messages)
	}
	if a.MaxTokens == 0 {
		t.Error("max_tokens should default to non-zero for Anthropic")
	}
}
