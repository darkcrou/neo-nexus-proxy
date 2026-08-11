package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// TransformToOpenAI converts an Anthropic request to OpenAI format
// Used when routing to OpenAI-compatible providers (DeepSeek, Groq, Gemini)
func TransformToOpenAI(anthropicReq AnthropicRequest, targetModel string) (OpenAIRequest, error) {
	oaiReq := OpenAIRequest{
		Model:     targetModel,
		MaxTokens: anthropicReq.MaxTokens,
		Stream:    anthropicReq.Stream,
	}

	// Convert messages
	for _, msg := range anthropicReq.Messages {
		oaiMsg, err := convertMessage(msg)
		if err != nil {
			return oaiReq, fmt.Errorf("failed to convert message: %w", err)
		}
		oaiReq.Messages = append(oaiReq.Messages, oaiMsg)
	}

	// Convert system prompt
	if anthropicReq.System != nil {
		switch s := anthropicReq.System.(type) {
		case string:
			oaiReq.Messages = append([]OpenAIMessage{{Role: "system", Content: textContent(s)}}, oaiReq.Messages...)
		case []interface{}:
			// System is an array of content blocks
			text := extractTextFromBlocks(s)
			if text != "" {
				oaiReq.Messages = append([]OpenAIMessage{{Role: "system", Content: textContent(text)}}, oaiReq.Messages...)
			}
		}
	}

	// Convert tools
	for _, tool := range anthropicReq.Tools {
		oaiTool, err := convertTool(tool)
		if err != nil {
			continue // skip tools that can't be converted
		}
		oaiReq.Tools = append(oaiReq.Tools, oaiTool)
	}

	return oaiReq, nil
}

// TransformFromOpenAI converts an OpenAI response back to Anthropic format
func TransformFromOpenAI(oaiResp OpenAIResponse, model string) AnthropicResponse {
	resp := AnthropicResponse{
		ID:   "msg_" + oaiResp.ID,
		Type: "message",
		Role: "assistant",
		Model: model,
		StopReason: mapStopReason(oaiResp.Choices[0].FinishReason),
		Usage: AnthropicUsage{
			InputTokens:  oaiResp.Usage.PromptTokens,
			OutputTokens: oaiResp.Usage.CompletionTokens,
		},
	}

	// Convert content
	choice := oaiResp.Choices[0]
	if choice.Message.Content.Text() != "" {
		resp.Content = []ContentBlock{{
			Type: "text",
			Text: choice.Message.Content.Text(),
		}}
	}

	// Convert tool calls
	for _, tc := range choice.Message.ToolCalls {
		var input interface{}
		json.Unmarshal([]byte(tc.Function.Arguments), &input)

		resp.Content = append(resp.Content, ContentBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: input,
		})
	}

	return resp
}

// ─── Helpers ───────────────────────────────────────────────────────────────

func convertMessage(msg Message) (OpenAIMessage, error) {
	oaiMsg := OpenAIMessage{Role: msg.Role}

	switch c := msg.Content.(type) {
	case string:
		oaiMsg.Content = textContent(c)
	case []interface{}:
		// Array of content blocks
		text := ""
		for _, block := range c {
			if b, ok := block.(map[string]interface{}); ok {
				switch b["type"] {
				case "text":
					if t, ok := b["text"].(string); ok {
						text += t
					}
				case "tool_result":
					// Handle tool results
					if content, ok := b["content"].([]interface{}); ok {
						text += extractTextFromBlocks(content)
					}
				case "tool_use":
					// Tool use in user message (for tool results)
					// Convert to tool call in OpenAI format
				}
			}
		}
		oaiMsg.Content = textContent(text)
	default:
		return oaiMsg, fmt.Errorf("unknown content type: %T", msg.Content)
	}

	return oaiMsg, nil
}

func convertTool(tool interface{}) (OpenAITool, error) {
	toolMap, ok := tool.(map[string]interface{})
	if !ok {
		return OpenAITool{}, fmt.Errorf("invalid tool format")
	}

	name, _ := toolMap["name"].(string)
	description, _ := toolMap["description"].(string)
	inputSchema, _ := toolMap["input_schema"].(map[string]interface{})

	return OpenAITool{
		Type: "function",
		Function: OpenAIFunction{
			Name:        name,
			Description: description,
			Parameters:  inputSchema,
		},
	}, nil
}

func extractTextFromBlocks(blocks []interface{}) string {
	text := ""
	for _, block := range blocks {
		if b, ok := block.(map[string]interface{}); ok {
			if b["type"] == "text" {
				if t, ok := b["text"].(string); ok {
					text += t
				}
			}
		}
	}
	return text
}

func mapStopReason(finishReason string) string {
	switch finishReason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	default:
		return "end_turn"
	}
}

// ─── OpenAI Types ──────────────────────────────────────────────────────────

type OpenAIRequest struct {
	Model         string               `json:"model"`
	Messages      []OpenAIMessage      `json:"messages"`
	MaxTokens     int                  `json:"max_tokens,omitempty"`
	Stream        bool                 `json:"stream,omitempty"`
	StreamOptions *OpenAIStreamOptions `json:"stream_options,omitempty"`
	Tools         []OpenAITool         `json:"tools,omitempty"`
}

// OpenAIStreamOptions asks the provider to include token usage in the final
// streaming chunk (so NEXUS can still track cost when streaming).
type OpenAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type OpenAIMessage struct {
	Role       string           `json:"role"`
	Content    OpenAIContent    `json:"content"`
	ToolCalls  []OpenAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

// OpenAIContent represents the OpenAI "content" field, which per spec is either a
// plain string or an array of content parts. NEXUS only supports the "text" part
// type — any other part type (image_url, input_audio, ...) is rejected during
// unmarshal rather than silently dropped.
//
// When the source was an array, the original parts are kept (not just the joined
// text) so callers that route to an Anthropic-format provider can forward the
// same block structure instead of collapsing it into one string — see Anthropic().
type OpenAIContent struct {
	text  string
	parts []OpenAIContentPart // non-nil only if the JSON source was an array
}

type OpenAIContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// textContent builds an OpenAIContent from a Go string, for call sites that
// already have flattened text (outbound requests to OpenAI-compatible providers,
// which always use the plain-string form).
func textContent(s string) OpenAIContent {
	return OpenAIContent{text: s}
}

func (c *OpenAIContent) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		c.text = s
		c.parts = nil
		return nil
	}
	var parts []OpenAIContentPart
	if err := json.Unmarshal(data, &parts); err != nil {
		return fmt.Errorf("content: expected a string or an array of content parts")
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.Type != "text" {
			return fmt.Errorf("content: unsupported content part type %q (only \"text\" is supported)", p.Type)
		}
		sb.WriteString(p.Text)
	}
	c.text = sb.String()
	c.parts = parts
	return nil
}

// MarshalJSON always emits the flattened string form. OpenAIMessage is only ever
// marshaled for the outbound-to-OpenAI-provider direction, where a plain string is
// universally accepted and is what NEXUS already constructs (see convertMessage).
func (c OpenAIContent) MarshalJSON() ([]byte, error) {
	return json.Marshal(c.text)
}

// Text returns the flattened text, regardless of whether the source JSON was a
// string or an array of text parts.
func (c OpenAIContent) Text() string { return c.text }

// Anthropic returns the value to embed in an Anthropic-format Message.Content
// field: the original array of text blocks when the source was an array
// (preserving structure, since Anthropic accepts the same block shape natively),
// or the plain string otherwise.
func (c OpenAIContent) Anthropic() interface{} {
	if c.parts == nil {
		return c.text
	}
	blocks := make([]map[string]interface{}, len(c.parts))
	for i, p := range c.parts {
		blocks[i] = map[string]interface{}{"type": "text", "text": p.Text}
	}
	return blocks
}

type OpenAITool struct {
	Type     string       `json:"type"`
	Function OpenAIFunction `json:"function"`
}

type OpenAIFunction struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
}

type OpenAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type OpenAIResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int           `json:"index"`
		Message      OpenAIMessage `json:"message"`
		FinishReason string        `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// ─── Anthropic Response Types ──────────────────────────────────────────────

type AnthropicResponse struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Role       string          `json:"role"`
	Model      string          `json:"model"`
	Content    []ContentBlock  `json:"content"`
	StopReason string          `json:"stop_reason"`
	Usage      AnthropicUsage  `json:"usage"`
}

type ContentBlock struct {
	Type  string      `json:"type"`
	Text  string      `json:"text,omitempty"`
	ID    string      `json:"id,omitempty"`
	Name  string      `json:"name,omitempty"`
	Input interface{} `json:"input,omitempty"`
}

type AnthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
}
