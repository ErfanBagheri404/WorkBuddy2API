package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// Server exposes OpenAI-compatible endpoints.
type Server struct {
	client *UpstreamClient
	auth   *AuthManager
	apiKey string
}

func NewServer(client *UpstreamClient, am *AuthManager) *Server {
	return &Server{client: client, auth: am}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/chat/completions", s.handleChat)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "unknown endpoint", "not_found")
	})
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	models, err := GetModels(s.auth)
	if err != nil {
		writeError(w, http.StatusBadGateway, "fetch models: "+err.Error(), "server_error")
		return
	}
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	now := time.Now().Unix()
	list := make([]model, 0, len(models))
	for _, m := range models {
		list = append(list, model{ID: m.ID, Object: "model", Created: now, OwnedBy: "workbuddy"})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": list})
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 10<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error(), "invalid_request_error")
		return
	}
	var probe struct {
		Model    string `json:"model"`
		Stream   *bool  `json:"stream"`
		Messages []any  `json:"messages"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		writeError(w, http.StatusBadRequest, "request body must be valid JSON", "invalid_request_error")
		return
	}
	if strings.TrimSpace(probe.Model) == "" {
		writeError(w, http.StatusBadRequest, "model is required", "invalid_request_error")
		return
	}
	if len(probe.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "messages must not be empty", "invalid_request_error")
		return
	}
	// WorkBuddy only supports streaming. Always request stream from upstream.
	// When the client wants non-streaming, buffer the SSE chunks and return
	// a single assembled JSON response.
	clientWantsStream := probe.Stream == nil || *probe.Stream
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	payload["stream"] = true
	body, _ = json.Marshal(payload)

	resp, err := s.client.Chat(body)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream: "+err.Error(), "server_error")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		copyBody(w, resp, resp.StatusCode)
		return
	}

	if clientWantsStream {
		copySSE(w, resp)
		return
	}

	// Collect all SSE chunks into a single non-stream response.
	// Each upstream choice gets its own accumulator keyed by choice index.
	type choiceAcc struct {
		collected []string
		toolCalls []assembledToolCall
		finish    string
	}
	accByIndex := map[int]*choiceAcc{}
	var (
		id    string
		model string
	)
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Choices []struct {
				Index int `json:"index"`
				Delta struct {
					Content   string `json:"content"`
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
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if chunk.ID != "" {
			id = chunk.ID
		}
		if chunk.Model != "" {
			model = chunk.Model
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
				if idx < 0 {
					continue
				}
				for len(acc.toolCalls) <= idx {
					acc.toolCalls = append(acc.toolCalls, assembledToolCall{Type: "function"})
				}
				if tc.ID != "" {
					acc.toolCalls[idx].ID = tc.ID
				}
				if tc.Type != "" {
					acc.toolCalls[idx].Type = tc.Type
				}
				if tc.Function.Name != "" {
					acc.toolCalls[idx].Function.Name = tc.Function.Name
				}
				acc.toolCalls[idx].Function.Arguments += tc.Function.Arguments
			}
			if c.FinishReason != nil {
				acc.finish = *c.FinishReason
			}
		}
	}
	// Emit one response choice per upstream choice.
	choices := make([]map[string]any, 0, len(accByIndex))
	for idx, acc := range accByIndex {
		if acc.finish == "" {
			acc.finish = "stop"
		}
		message := map[string]any{
			"role":    "assistant",
			"content": strings.Join(acc.collected, ""),
		}
		if len(acc.toolCalls) > 0 {
			message["tool_calls"] = acc.toolCalls
			if acc.finish == "stop" {
				acc.finish = "tool_calls"
			}
		}
		choices = append(choices, map[string]any{
			"index":         idx,
			"message":       message,
			"finish_reason": acc.finish,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"model":   model,
		"choices": choices,
	})
}

// assembledToolCall is the non-streaming form of a tool call rebuilt from
// streamed deltas.
type assembledToolCall struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func writeError(w http.ResponseWriter, status int, msg, typ string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": typ},
	})
}
