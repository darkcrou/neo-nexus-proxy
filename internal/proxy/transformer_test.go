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
		{name: "unsupported part type is rejected", json: `[{"type":"input_audio","input_audio":{"data":"x","format":"wav"}}]`, wantErr: `supported: "text", "image_url"`},
		{name: "unsupported part type names the type", json: `[{"type":"input_audio"}]`, wantErr: "input_audio"},
		{name: "image data uri accepted", json: `[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]`, want: "a"},
		{name: "image https url accepted", json: `[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]`, want: ""},
		{name: "image http url accepted", json: `[{"type":"image_url","image_url":{"url":"http://example.com/a.png"}}]`, want: ""},
		{name: "image ftp url rejected", json: `[{"type":"image_url","image_url":{"url":"ftp://example.com/a.png"}}]`, wantErr: "invalid url"},
		{name: "image non-base64 data uri rejected", json: `[{"type":"image_url","image_url":{"url":"data:text/plain,abc"}}]`, wantErr: "invalid url"},
		{name: "image data uri with empty payload rejected", json: `[{"type":"image_url","image_url":{"url":"data:image/png;base64,"}}]`, wantErr: "invalid url"},
		{name: "image data uri without mime rejected", json: `[{"type":"image_url","image_url":{"url":"data:;base64,AAAA"}}]`, wantErr: "invalid url"},
		{name: "image empty url rejected", json: `[{"type":"image_url","image_url":{"url":""}}]`, wantErr: "invalid url"},
		{name: "image part missing image_url object rejected", json: `[{"type":"image_url"}]`, wantErr: "invalid url"},
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

func TestOpenAIContent_ImageDetailPreserved(t *testing.T) {
	var c OpenAIContent
	in := `[{"type":"image_url","image_url":{"url":"https://example.com/a.png","detail":"high"}}]`
	if err := json.Unmarshal([]byte(in), &c); err != nil {
		t.Fatal(err)
	}
	if c.parts[0].ImageURL.Detail != "high" {
		t.Errorf("detail = %q, want high", c.parts[0].ImageURL.Detail)
	}
	out, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"detail":"high"`) {
		t.Errorf("marshaled content lost detail: %s", out)
	}
}

func TestOpenAIContent_Anthropic_ImageBlocksInOrder(t *testing.T) {
	var c OpenAIContent
	in := `[{"type":"text","text":"look"},
		{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,/9j/4AAQ"}},
		{"type":"image_url","image_url":{"url":"https://example.com/b.png"}}]`
	if err := json.Unmarshal([]byte(in), &c); err != nil {
		t.Fatal(err)
	}
	blocks, ok := c.Anthropic().([]map[string]interface{})
	if !ok || len(blocks) != 3 {
		t.Fatalf("Anthropic() = %#v, want 3 blocks", c.Anthropic())
	}
	if blocks[0]["type"] != "text" || blocks[0]["text"] != "look" {
		t.Errorf("block[0] = %+v", blocks[0])
	}
	src1, _ := blocks[1]["source"].(map[string]interface{})
	if blocks[1]["type"] != "image" || src1["type"] != "base64" || src1["media_type"] != "image/jpeg" || src1["data"] != "/9j/4AAQ" {
		t.Errorf("block[1] = %+v", blocks[1])
	}
	src2, _ := blocks[2]["source"].(map[string]interface{})
	if blocks[2]["type"] != "image" || src2["type"] != "url" || src2["url"] != "https://example.com/b.png" {
		t.Errorf("block[2] = %+v", blocks[2])
	}
}

func TestOpenAIContent_MarshalKeepsImages(t *testing.T) {
	var c OpenAIContent
	in := `[{"type":"text","text":"x"},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]`
	if err := json.Unmarshal([]byte(in), &c); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), "[") || !strings.Contains(string(out), `"image_url":{"url":"https://example.com/a.png"}`) {
		t.Errorf("images lost on marshal: %s", out)
	}
}

func TestOpenAIContent_MarshalTextOnlyUnchanged(t *testing.T) {
	var c OpenAIContent
	if err := json.Unmarshal([]byte(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`), &c); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(c)
	if string(out) != `"ab"` {
		t.Errorf("text-only content marshal = %s, want \"ab\"", out)
	}
	out, _ = json.Marshal(textContent("hi"))
	if string(out) != `"hi"` {
		t.Errorf("textContent marshal = %s", out)
	}
}
