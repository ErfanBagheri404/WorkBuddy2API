package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// --- Anthropic response helpers ---

// writeAnthropicError sends an Anthropic-shaped error response.
func writeAnthropicError(w http.ResponseWriter, code int, msg, errType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": msg,
		},
	})
}

// anthropicNonStream reads the full upstream OpenAI SSE stream, assembles it,
// and returns a single Anthropic Messages response.
func anthropicNonStream(w http.ResponseWriter, resp *http.Response, ar *anthropicRequest, cid string) {
	type choiceAcc struct {
		collected []string
		toolCalls []anthropicToolCallAcc
		finish    string
	}
	accByIndex := map[int]*choiceAcc{}
	var id string

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var chunk openaiChunk
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if chunk.ID != "" {
			id = chunk.ID
		}
		for _, c := range chunk.Choices {
			acc, ok := accByIndex[c.Index]
			if !ok {
				acc = &choiceAcc{}
				accByIndex[c.Index] = acc
			}
			if c.Delta.Content != "" {
				acc.collected = append(acc.collected, c.Delta.Content)
			}
			for _, tc := range c.Delta.ToolCalls {
				idx := len(acc.toolCalls)
				if tc.Index != nil {
					idx = *tc.Index
				}
				for len(acc.toolCalls) <= idx {
					acc.toolCalls = append(acc.toolCalls, anthropicToolCallAcc{})
				}
				if tc.ID != "" {
					acc.toolCalls[idx].ID = tc.ID
				}
				if tc.Function.Name != "" {
					acc.toolCalls[idx].Name = tc.Function.Name
				}
				acc.toolCalls[idx].Args += tc.Function.Arguments
			}
			if c.FinishReason != nil {
				acc.finish = *c.FinishReason
			}
		}
	}
	if err := scanner.Err(); err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "stream error: "+err.Error(), "api_error")
		return
	}

	// Build Anthropic content blocks
	acc := accByIndex[0]
	if acc == nil {
		acc = &choiceAcc{}
	}
	content := []any{}
	if text := strings.Join(acc.collected, ""); text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for i, tc := range acc.toolCalls {
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    fmt.Sprintf("toolu_%d_%s", i, id),
			"name":  tc.Name,
			"input": json.RawMessage(tc.Args),
		})
	}

	stopReason := "end_turn"
	switch acc.finish {
	case "tool_calls", "function_call":
		stopReason = "tool_use"
	case "length":
		stopReason = "max_tokens"
	}

	inputTokens := estimateTokens(ar.Messages)
	outputTokens := len(strings.Join(acc.collected, "")) / 4
	if outputTokens == 0 {
		outputTokens = 1
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id":         id,
		"type":       "message",
		"role":       "assistant",
		"content":    content,
		"model":      ar.Model,
		"stop_reason": stopReason,
		"usage": map[string]any{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
		},
	})
}

// anthropicStream pipes the upstream OpenAI SSE stream to the client in
// Anthropic's event format: message_start → content_block_delta(s) →
// content_block_stop → message_delta → message_stop.
func anthropicStream(w http.ResponseWriter, resp *http.Response, ar *anthropicRequest, cid string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)

	msgID := "msg_" + cid

	// Emit message_start
	startMsg := map[string]any{
		"type":  "message_start",
		"message": map[string]any{
			"id":         msgID,
			"type":       "message",
			"role":       "assistant",
			"content":    []any{},
			"model":      ar.Model,
			"stop_reason": nil,
			"usage":      map[string]any{"input_tokens": estimateTokens(ar.Messages), "output_tokens": 0},
		},
	}
	writeSSEEvent(w, "message_start", startMsg)
	if flusher != nil {
		flusher.Flush()
	}

	textBlockIdx := 0
	inToolUse := false
	toolIdx := 0

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var chunk openaiChunk
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		for _, c := range chunk.Choices {
			// Text content delta
			if c.Delta.Content != "" {
				if inToolUse {
					// Close the tool_use block, start a new text block
					writeSSEEvent(w, "content_block_stop", map[string]any{
						"type":  "content_block_stop",
						"index": textBlockIdx,
					})
					textBlockIdx++
					inToolUse = false
				}
				writeSSEEvent(w, "content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": textBlockIdx,
					"delta": map[string]any{
						"type": "text_delta",
						"text": c.Delta.Content,
					},
				})
				if flusher != nil {
					flusher.Flush()
				}
			}
			// Tool call deltas
			for _, tc := range c.Delta.ToolCalls {
				if tc.ID != "" {
					// New tool call starting
					if inToolUse {
						writeSSEEvent(w, "content_block_stop", map[string]any{
							"type":  "content_block_stop",
							"index": textBlockIdx,
						})
						textBlockIdx++
					}
					inToolUse = true
					toolIdx = textBlockIdx

					// Emit content_block_start for tool_use
					writeSSEEvent(w, "content_block_start", map[string]any{
						"type":  "content_block_start",
						"index": textBlockIdx,
						"content_block": map[string]any{
							"type":  "tool_use",
							"id":    tc.ID,
							"name":  tc.Function.Name,
							"input": map[string]any{},
						},
					})
				}
				if tc.Function.Arguments != "" {
					writeSSEEvent(w, "content_block_delta", map[string]any{
						"type":  "content_block_delta",
						"index": toolIdx,
						"delta": map[string]any{
							"type":        "input_json_delta",
							"partial_json": tc.Function.Arguments,
						},
					})
					if flusher != nil {
						flusher.Flush()
					}
				}
			}
			// Finish reason → message_delta
			if c.FinishReason != nil {
				if inToolUse {
					writeSSEEvent(w, "content_block_stop", map[string]any{
						"type":  "content_block_stop",
						"index": textBlockIdx,
					})
					inToolUse = false
				}
				stopReason := "end_turn"
				switch *c.FinishReason {
				case "tool_calls", "function_call":
					stopReason = "tool_use"
				case "length":
					stopReason = "max_tokens"
				}
				writeSSEEvent(w, "message_delta", map[string]any{
					"type":         "message_delta",
					"delta":        map[string]any{"stop_reason": stopReason},
					"usage":        map[string]any{"output_tokens": 1},
				})
				writeSSEEvent(w, "message_stop", map[string]any{
					"type": "message_stop",
				})
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
	}
}

func writeSSEEvent(w io.Writer, event string, data any) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(b))
}

// openaiChunk mirrors the upstream OpenAI SSE chunk format.
type openaiChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content string `json:"content"`
			ToolCalls []struct {
				Index    *int   `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

type anthropicToolCallAcc struct {
	ID   string
	Name string
	Args string
}

func estimateTokens(msgs []anthropicMsg) int {
	chars := 0
	for _, m := range msgs {
		b, _ := json.Marshal(m)
		chars += len(b)
	}
	tokens := chars / 4
	if tokens < 1 {
		tokens = 1
	}
	return tokens
}
