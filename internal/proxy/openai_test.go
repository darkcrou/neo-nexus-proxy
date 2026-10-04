package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lynuxis2026-pixel/nexus-proxy/internal/providers"
	"github.com/lynuxis2026-pixel/nexus-proxy/internal/router"
)

func TestRelayOpenAIStream(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":"Hello "},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":"world"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2}}`,
		`data: [DONE]`,
	}, "\n") + "\n"

	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(sse)), Header: http.Header{}}
	h := &Handler{} // nil db/broker/budget — logResult is a no-op
	active := &activeProvider{impl: providers.NewGroq("k")}
	rec := httptest.NewRecorder()

	// nil att — this test exercises the relay's SSE synthesis, not events
	h.relayOpenAIStream(rec, httptest.NewRequest("POST", "/v1/messages", nil), active, AnthropicRequest{Model: "claude-haiku-4-5", Stream: true}, resp, time.Now(), router.ComplexitySimple, nil)

	out := rec.Body.String()
	for _, want := range []string{"event: message_start", "content_block_start", "text_delta", "Hello ", "world", "event: message_stop"} {
		if !strings.Contains(out, want) {
			t.Errorf("stream output missing %q\n--- got ---\n%s", want, out)
		}
	}
}

func TestTransformFromOpenAI(t *testing.T) {
	raw := `{
		"id": "chatcmpl-abc",
		"model": "deepseek-chat",
		"choices": [{"index": 0, "message": {"role": "assistant", "content": "Hello world"}, "finish_reason": "stop"}],
		"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
	}`
	var oai OpenAIResponse
	if err := json.Unmarshal([]byte(raw), &oai); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	resp := TransformFromOpenAI(oai, "claude-haiku-4-5")

	if resp.Model != "claude-haiku-4-5" {
		t.Errorf("model = %q, want claude-haiku-4-5", resp.Model)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", resp.StopReason)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != "text" || resp.Content[0].Text != "Hello world" {
		t.Errorf("content = %+v, want one text block 'Hello world'", resp.Content)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 5 {
		t.Errorf("usage = %+v, want in=10 out=5", resp.Usage)
	}
}

func TestWriteAnthropicSSE(t *testing.T) {
	rec := httptest.NewRecorder()
	resp := AnthropicResponse{
		ID:         "msg_1",
		Type:       "message",
		Role:       "assistant",
		Model:      "claude-haiku-4-5",
		StopReason: "end_turn",
		Content:    []ContentBlock{{Type: "text", Text: "Hi there"}},
		Usage:      AnthropicUsage{InputTokens: 3, OutputTokens: 2},
	}

	writeAnthropicSSE(rec, "groq", resp)

	out := rec.Body.String()
	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		"text_delta",
		"Hi there",
		"event: content_block_stop",
		"event: message_delta",
		"event: message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("SSE output missing %q\n--- output ---\n%s", want, out)
		}
	}
	if got := rec.Header().Get("X-Nexus-Provider"); got != "groq" {
		t.Errorf("X-Nexus-Provider = %q, want groq", got)
	}
}

func TestParseAnthropicUsage(t *testing.T) {
	body := []byte(`{"type":"message","usage":{"input_tokens":42,"output_tokens":17}}`)
	in, out := parseAnthropicUsage(body)
	if in != 42 || out != 17 {
		t.Errorf("parseAnthropicUsage = (%d,%d), want (42,17)", in, out)
	}
}

func TestBudgetTracker(t *testing.T) {
	if newBudgetTracker(0, 0, "", 0).Over() {
		t.Error("zero limit means unlimited — never over budget")
	}
	b := newBudgetTracker(1.0, 0, "", 0)
	if b.Over() {
		t.Error("0 spend should not be over")
	}
	b.Add(0.5)
	if b.Over() {
		t.Error("0.5 < 1.0 should not be over")
	}
	b.Add(0.6)
	if !b.Over() {
		t.Error("1.1 >= 1.0 should be over budget")
	}
}

func TestFreeLocalOnly(t *testing.T) {
	chain := []*router.Provider{
		{Name: "anthropic", Tier: "premium"},
		{Name: "groq", Tier: "free"},
		{Name: "ollama", Tier: "local"},
		{Name: "deepseek", Tier: "standard"},
	}
	got := freeLocalOnly(chain)
	if len(got) != 2 {
		t.Fatalf("expected 2 free/local providers, got %d", len(got))
	}
	for _, p := range got {
		if p.Tier != "free" && p.Tier != "local" {
			t.Errorf("unexpected tier %q in filtered chain", p.Tier)
		}
	}
}

func TestParseStreamUsage(t *testing.T) {
	stream := []byte(`event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":100,"output_tokens":1}}}

event: message_delta
data: {"type":"message_delta","usage":{"output_tokens":58}}
`)
	in, out := parseStreamUsage(stream)
	if in != 100 || out != 58 {
		t.Errorf("parseStreamUsage = (%d,%d), want (100,58)", in, out)
	}
}
