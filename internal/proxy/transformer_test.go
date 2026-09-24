package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOpenAIContent_UnmarshalJSON(t *testing.T) {
	cases := []struct {
		name    string
		json    string
		want    string
		wantErr string
	}{
		{name: "plain string", json: `"hello"`, want: "hello"},
		{name: "single text part", json: `[{"type":"text","text":"hi"}]`, want: "hi"},
		{name: "multiple text parts join in order", json: `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, want: "ab"},
		{name: "unsupported part type is rejected", json: `[{"type":"image_url","image_url":{"url":"x"}}]`, wantErr: "image_url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var c OpenAIContent
			err := json.Unmarshal([]byte(tc.json), &c)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.Text() != tc.want {
				t.Errorf("Text() = %q, want %q", c.Text(), tc.want)
			}
		})
	}
}

func TestTransformToOpenAI_StringMessages(t *testing.T) {
	req := AnthropicRequest{
		Model:     "claude-haiku-4-5",
		MaxTokens: 100,
		Messages:  []Message{{Role: "user", Content: "hello"}},
	}
	oai, err := TransformToOpenAI(req, "llama-3.3-70b-versatile")
	if err != nil {
		t.Fatal(err)
	}
	if oai.Model != "llama-3.3-70b-versatile" {
		t.Errorf("model = %q", oai.Model)
	}
	if len(oai.Messages) != 1 || oai.Messages[0].Role != "user" || oai.Messages[0].Content.Text() != "hello" {
		t.Errorf("messages = %+v", oai.Messages)
	}
}

func TestTransformToOpenAI_SystemString(t *testing.T) {
	req := AnthropicRequest{
		Model:    "m",
		System:   "be helpful",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}
	oai, _ := TransformToOpenAI(req, "x")
	if len(oai.Messages) != 2 || oai.Messages[0].Role != "system" || oai.Messages[0].Content.Text() != "be helpful" {
		t.Errorf("system message not prepended: %+v", oai.Messages)
	}
}

func TestTransformToOpenAI_ContentBlocks(t *testing.T) {
	req := AnthropicRequest{
		Model: "m",
		Messages: []Message{{Role: "user", Content: []interface{}{
			map[string]interface{}{"type": "text", "text": "part1 "},
			map[string]interface{}{"type": "text", "text": "part2"},
		}}},
	}
	oai, _ := TransformToOpenAI(req, "x")
	if oai.Messages[0].Content.Text() != "part1 part2" {
		t.Errorf("content blocks not concatenated: %q", oai.Messages[0].Content.Text())
	}
}

// TestTransformToOpenAI_TextOnlyContentBlocksUnchanged pins down that a
// content-block array with no images still produces the plain-string content
// form (not a parts array) — zero behavior change for existing text-only
// provider paths.
func TestTransformToOpenAI_TextOnlyContentBlocksUnchanged(t *testing.T) {
	req := AnthropicRequest{
		Model: "m",
		Messages: []Message{{Role: "user", Content: []interface{}{
			map[string]interface{}{"type": "text", "text": "part1 "},
			map[string]interface{}{"type": "text", "text": "part2"},
		}}},
	}
	oai, err := TransformToOpenAI(req, "x")
	if err != nil {
		t.Fatal(err)
	}
	got := oai.Messages[0].Content
	if got.asArray {
		t.Fatalf("expected plain-string content, got array form: %+v", got)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"part1 part2"` {
		t.Errorf("marshaled content = %s, want %q", b, `"part1 part2"`)
	}
}

// TestTransformToOpenAI_Base64ImageBlock covers a text block followed by a
// base64-source image block: the result must be a 2-part ordered array, text
// first, then an image_url part with a correctly formed data URI.
func TestTransformToOpenAI_Base64ImageBlock(t *testing.T) {
	req := AnthropicRequest{
		Model: "m",
		Messages: []Message{{Role: "user", Content: []interface{}{
			map[string]interface{}{"type": "text", "text": "what is this?"},
			map[string]interface{}{
				"type": "image",
				"source": map[string]interface{}{
					"type":       "base64",
					"media_type": "image/png",
					"data":       "aGVsbG8=",
				},
			},
		}}},
	}
	oai, err := TransformToOpenAI(req, "x")
	if err != nil {
		t.Fatal(err)
	}
	got := oai.Messages[0].Content
	if !got.asArray {
		t.Fatalf("expected array content form, got plain string: %+v", got)
	}
	if len(got.parts) != 2 {
		t.Fatalf("expected 2 parts, got %d: %+v", len(got.parts), got.parts)
	}
	if got.parts[0].Type != "text" || got.parts[0].Text != "what is this?" {
		t.Errorf("part[0] = %+v", got.parts[0])
	}
	if got.parts[1].Type != "image_url" || got.parts[1].ImageURL == nil {
		t.Fatalf("part[1] = %+v, want an image_url part", got.parts[1])
	}
	wantURL := "data:image/png;base64,aGVsbG8="
	if got.parts[1].ImageURL.URL != wantURL {
		t.Errorf("image_url.url = %q, want %q", got.parts[1].ImageURL.URL, wantURL)
	}
}

// TestTransformToOpenAI_URLImageBlock covers a url-source image block: the
// url must pass through to image_url.url verbatim.
func TestTransformToOpenAI_URLImageBlock(t *testing.T) {
	req := AnthropicRequest{
		Model: "m",
		Messages: []Message{{Role: "user", Content: []interface{}{
			map[string]interface{}{
				"type": "image",
				"source": map[string]interface{}{
					"type": "url",
					"url":  "https://example.com/cat.png",
				},
			},
		}}},
	}
	oai, err := TransformToOpenAI(req, "x")
	if err != nil {
		t.Fatal(err)
	}
	got := oai.Messages[0].Content
	if len(got.parts) != 1 || got.parts[0].Type != "image_url" || got.parts[0].ImageURL == nil {
		t.Fatalf("parts = %+v, want a single image_url part", got.parts)
	}
	if got.parts[0].ImageURL.URL != "https://example.com/cat.png" {
		t.Errorf("image_url.url = %q", got.parts[0].ImageURL.URL)
	}
}

// TestTransformToOpenAI_ToolResultNestedImage covers a tool_result block
// whose own content array contains an image (e.g. a custom tool that returns
// a screenshot) — the nested image must surface as an image_url part in the
// outer parts array.
func TestTransformToOpenAI_ToolResultNestedImage(t *testing.T) {
	req := AnthropicRequest{
		Model: "m",
		Messages: []Message{{Role: "user", Content: []interface{}{
			map[string]interface{}{
				"type":        "tool_result",
				"tool_use_id": "toolu_1",
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "screenshot:"},
					map[string]interface{}{
						"type": "image",
						"source": map[string]interface{}{
							"type":       "base64",
							"media_type": "image/jpeg",
							"data":       "Zm9v",
						},
					},
				},
			},
		}}},
	}
	oai, err := TransformToOpenAI(req, "x")
	if err != nil {
		t.Fatal(err)
	}
	got := oai.Messages[0].Content
	if !got.asArray {
		t.Fatalf("expected array content form, got plain string: %+v", got)
	}
	if len(got.parts) != 2 {
		t.Fatalf("expected 2 parts, got %d: %+v", len(got.parts), got.parts)
	}
	if got.parts[0].Type != "text" || got.parts[0].Text != "screenshot:" {
		t.Errorf("part[0] = %+v", got.parts[0])
	}
	wantURL := "data:image/jpeg;base64,Zm9v"
	if got.parts[1].Type != "image_url" || got.parts[1].ImageURL == nil || got.parts[1].ImageURL.URL != wantURL {
		t.Errorf("part[1] = %+v, want image_url with url %q", got.parts[1], wantURL)
	}
}

func TestMapStopReason(t *testing.T) {
	cases := map[string]string{
		"stop":       "end_turn",
		"length":     "max_tokens",
		"tool_calls": "tool_use",
		"unexpected": "end_turn",
	}
	for in, want := range cases {
		if got := mapStopReason(in); got != want {
			t.Errorf("mapStopReason(%q) = %q, want %q", in, got, want)
		}
	}
}
