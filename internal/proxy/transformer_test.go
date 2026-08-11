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
