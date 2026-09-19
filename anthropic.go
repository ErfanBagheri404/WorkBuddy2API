package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// --- Anthropic request → OpenAI payload translation ---

// anthropicRequest mirrors the Anthropic Messages API request shape.
type anthropicRequest struct {
	Model     string          `json:"model"`
	Messages  []anthropicMsg  `json:"messages"`
	System    json.RawMessage `json:"system,omitempty"`
	MaxTokens int             `json:"max_tokens"`
	Stream    *bool           `json:"stream,omitempty"`
	Tools     []anthropicTool `json:"tools,omitempty"`
}

type anthropicMsg struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string or []ContentBlock
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// contentBlock is a generic block in an Anthropic message.
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// tool_use fields
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name,omitempty"`
	Input any            `json:"input,omitempty"`
	// tool_result fields
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   any    `json:"content,omitempty"`
}

// anthropicToOpenAI converts an Anthropic /v1/messages request into an
// OpenAI chat completion payload suitable for WorkBuddy upstream.
func anthropicToOpenAI(ar *anthropicRequest) map[string]any {
	payload := map[string]any{
		"model":       ar.Model,
		"max_tokens":  ar.MaxTokens,
		"stream":      true,
	}

	// System prompt: Anthropic uses a top-level "system" field.
	if len(ar.System) > 0 {
		var sysText string
		if json.Unmarshal(ar.System, &sysText) == nil {
			payload["messages"] = []any{
				map[string]any{"role": "system", "content": sysText},
			}
		} else {
			// Array of content blocks
			var blocks []contentBlock
			if json.Unmarshal(ar.System, &blocks) == nil {
				var parts []string
				for _, b := range blocks {
					if b.Type == "text" {
						parts = append(parts, b.Text)
					}
				}
				payload["messages"] = []any{
					map[string]any{"role": "system", "content": strings.Join(parts, "\n")},
				}
			}
		}
	}

	// Convert messages
	msgs := make([]any, 0, len(ar.Messages))
	for _, m := range ar.Messages {
		switch content := m.Content.(type) {
		case string:
			msgs = append(msgs, map[string]any{"role": m.Role, "content": content})
		case []any:
			// Array of content blocks — flatten to OpenAI format
			textParts := []string{}
			var toolCalls []map[string]any
			var toolResults []map[string]any
			for _, raw := range content {
				b, _ := json.Marshal(raw)
				var blk contentBlock
				json.Unmarshal(b, &blk)
				switch blk.Type {
				case "text":
					textParts = append(textParts, blk.Text)
				case "tool_use":
					toolCalls = append(toolCalls, map[string]any{
						"id":   blk.ID,
						"type": "function",
						"function": map[string]any{
							"name":      blk.Name,
							"arguments": marshalJSON(blk.Input),
						},
					})
				case "tool_result":
					toolResults = append(toolResults, map[string]any{
						"tool_call_id": blk.ToolUseID,
						"content":      formatToolResultContent(blk.Content),
					})
				}
			}
			out := map[string]any{"role": m.Role}
			if len(textParts) > 0 {
				out["content"] = strings.Join(textParts, "\n")
			}
			if len(toolCalls) > 0 {
				out["tool_calls"] = toolCalls
				out["role"] = "assistant"
			}
			if len(toolResults) > 0 {
				// OpenAI expects individual tool messages
				for _, tr := range toolResults {
					tr["role"] = "tool"
					msgs = append(msgs, tr)
				}
				continue
			}
			msgs = append(msgs, out)
		default:
			msgs = append(msgs, map[string]any{"role": m.Role, "content": fmt.Sprintf("%v", content)})
		}
	}
	payload["messages"] = msgs

	// Convert tools
	if len(ar.Tools) > 0 {
		tools := make([]map[string]any, 0, len(ar.Tools))
		for _, t := range ar.Tools {
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  json.RawMessage(t.InputSchema),
				},
			})
		}
		payload["tools"] = tools
	}

	return payload
}

func formatToolResultContent(content any) string {
	if content == nil {
		return ""
	}
	switch v := content.(type) {
	case string:
		return v
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

func marshalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
