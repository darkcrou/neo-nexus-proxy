package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lynuxis2026-pixel/nexus-proxy/internal/providers"
	"github.com/lynuxis2026-pixel/nexus-proxy/internal/router"
)

func decodeJSONValue(t *testing.T, s string) interface{} {
	t.Helper()
	var v interface{}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("bad test JSON %q: %v", s, err)
	}
	return v
}

func TestContentHasImage(t *testing.T) {
	// nest wraps an image block in n levels of tool_result content.
	nest := func(n int) string {
		s := `[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}]`
		for i := 0; i < n; i++ {
			s = `[{"type":"tool_result","tool_use_id":"t","content":` + s + `}]`
		}
		return s
	}
	cases := []struct {
		name string
		json string
		want bool
	}{
		{"string content", `"just text"`, false},
		{"null", `null`, false},
		{"text only", `[{"type":"text","text":"hi"}]`, false},
		{"tool_result with string content", `[{"type":"tool_result","tool_use_id":"t","content":"ok"}]`, false},
		{"anthropic image", `[{"type":"text","text":"a"},{"type":"image","source":{"type":"url","url":"http://x/a.png"}}]`, true},
		{"openai image_url", `[{"type":"image_url","image_url":{"url":"data:image/png;base64,aGk="}}]`, true},
		{"openai input_image", `[{"type":"input_image","image_url":"data:image/png;base64,aGk="}]`, true},
		{"image nested in tool_result", nest(1), true},
		{"image nested within cap", nest(maxImageScanDepth), true},
		{"image nested beyond cap does not panic", nest(maxImageScanDepth + 50), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := contentHasImage(decodeJSONValue(t, tc.json)); got != tc.want {
				t.Errorf("contentHasImage = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMessagesHaveImage(t *testing.T) {
	var msgs []map[string]interface{}
	if err := json.Unmarshal([]byte(`[{"role":"user","content":"hi"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}]}]}]`), &msgs); err != nil {
		t.Fatal(err)
	}
	if !messagesHaveImage(msgs) {
		t.Error("messagesHaveImage = false, want true for a tool_result-nested image")
	}
	if messagesHaveImage(msgs[:1]) {
		t.Error("messagesHaveImage = true for a string-only message")
	}
}

func TestPromptText_HasImages(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"openai image_url", `{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"text","text":"q"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGk="}}]}]}`, true},
		{"tool_result nested image", `{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}},{"type":"text","text":"q"}]}]}]}`, true},
		{"image in system", `{"model":"m","system":[{"type":"image","source":{"type":"url","url":"http://x/a.png"}},{"type":"text","text":"s"}],"messages":[{"role":"user","content":"q"}]}`, true},
		{"text only", `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"q"}]}]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, hasImages, _ := promptText([]byte(tc.body))
			if hasImages != tc.want {
				t.Errorf("hasImages = %v, want %v", hasImages, tc.want)
			}
		})
	}
}

// TestSemanticCacheSkipsImageURLRequests: two OpenAI-gateway requests with the
// same text but different image_url data URIs must reach the upstream twice
// and never be a semantic HIT against each other.
func TestSemanticCacheSkipsImageURLRequests(t *testing.T) {
	var calls int
	srv := semanticMock(&calls)
	defer srv.Close()

	h := buildTestHandler(t, []testProv{{"p", "free", srv.URL}})
	h.cache = newResponseCache(time.Minute, 100, true, 0.5)

	body := func(data string) string {
		return `{"model":"gpt-4o","messages":[{"role":"user","content":[` +
			`{"type":"text","text":"what is in this image?"},` +
			`{"type":"image_url","image_url":{"url":"data:image/png;base64,` + data + `"}}]}]}`
	}

	rec1 := chatCompletions(h, body("aGVsbG8xMjM0NTY3ODkwYWJjZGVmZ2g="))
	rec2 := chatCompletions(h, body("d29ybGQ5ODc2NTQzMjEwemyeHd2dXQ="))

	if rec1.Code != 200 || rec2.Code != 200 {
		t.Fatalf("status = %d / %d, bodies = %s / %s", rec1.Code, rec2.Code, rec1.Body.String(), rec2.Body.String())
	}
	if calls != 2 {
		t.Errorf("upstream calls = %d, want 2 (image_url requests must skip the semantic cache)", calls)
	}
	if rec1.Header().Get("X-Nexus-Cache") == "HIT" || rec2.Header().Get("X-Nexus-Cache") == "HIT" {
		t.Errorf("image_url request served as a cache HIT (%q / %q)", rec1.Header().Get("X-Nexus-Cache"), rec2.Header().Get("X-Nexus-Cache"))
	}
}

// TestHandleMessages_ToolResultImage_UsesVisionModelOverride: an image that
// only appears inside a tool_result (how Claude Code delivers images read from
// disk) still triggers the provider's vision_model override.
func TestHandleMessages_ToolResultImage_UsesVisionModelOverride(t *testing.T) {
	var gotModel string
	srv := captureModelServer(&gotModel)
	defer srv.Close()

	impl, err := providers.New(providers.Spec{
		Name: "visionprov", Type: "openai-compatible", BaseURL: srv.URL,
		Models: []string{"llama-x"}, VisionModel: "llama-vision-90b",
	})
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(router.StrategyAuto)
	rt.AddProvider(&router.Provider{Name: "visionprov", Tier: "free", Healthy: true})
	h := &Handler{httpClient: &http.Client{Timeout: 10 * time.Second}, router: rt, providers: map[string]*activeProvider{"visionprov": {impl: impl, apiKey: "k"}}}

	body := `{"model":"claude-sonnet-4-6","max_tokens":50,"messages":[` +
		`{"role":"user","content":"look at the file"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"a.png"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]}]}]}`
	rec := doMessages(h, body)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotModel != "llama-vision-90b" {
		t.Errorf("upstream model = %q, want vision_model override %q", gotModel, "llama-vision-90b")
	}
}

func TestElideImageData(t *testing.T) {
	long := strings.Repeat("QUJD", 100) // 400 base64 chars
	short := strings.Repeat("A", 127)
	cases := []struct{ name, in, want string }{
		{"short data untouched", `{"source":{"data":"aGk="}}`, `{"source":{"data":"aGk="}}`},
		{"just under threshold untouched", `{"data":"` + short + `"}`, `{"data":"` + short + `"}`},
		{"long base64 replaced", `{"type":"base64","data":"` + long + `","x":1}`, `{"type":"base64","data":"[image data omitted: 400 bytes]","x":1}`},
		{"padded and url-safe alphabet", `{"data":"` + strings.Repeat("a-_", 50) + `=="}`, `{"data":"[image data omitted: 152 bytes]"}`},
		{"two images", `[{"data":"` + long + `"},{"data":"` + long + `"}]`, `[{"data":"[image data omitted: 400 bytes]"},{"data":"[image data omitted: 400 bytes]"}]`},
		{"long non-data text untouched", `{"text":"` + long + `"}`, `{"text":"` + long + `"}`},
		{"long non-base64 data untouched", `{"data":"` + strings.Repeat("hello world ", 40) + `"}`, `{"data":"` + strings.Repeat("hello world ", 40) + `"}`},
		{"plain text untouched", `{"messages":[{"role":"user","content":"hi"}]}`, `{"messages":[{"role":"user","content":"hi"}]}`},
	}
	for _, c := range cases {
		if got := elideImageData(c.in); got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.name, got, c.want)
		}
	}
}
