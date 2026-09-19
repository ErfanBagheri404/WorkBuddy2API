package main

import (
	"bufio"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// Server exposes OpenAI-compatible endpoints.
type Server struct {
	client      *UpstreamClient
	auth        *AuthManager
	apiKey      string
	desensitize bool
	limiter     *RateLimiter
}

func NewServer(client *UpstreamClient, am *AuthManager, apiKey string, desensitize bool, limiter *RateLimiter) *Server {
	return &Server{client: client, auth: am, apiKey: apiKey, desensitize: desensitize, limiter: limiter}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/models/", s.handleModels)   // catches /v1/models/<id>
	mux.HandleFunc("/v1/chat/completions", s.handleChat)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "unknown endpoint", "not_found")
	})
	// Auth applies to the whole /v1/ space so an unknown /v1/* path 401s
	// rather than leaking 404 to unauthenticated callers.
	var h http.Handler = s.requireAPIKey(mux)
	if s.limiter != nil {
		h = s.limiter.middleware(h)
	}
	return h
}

// requireAPIKey enforces bearer-token auth on the /v1/ route space when a key
// is configured. Paths outside /v1/ (notably /healthz) stay open, and when no
// key is set every request passes through, preserving the original
// unauthenticated localhost-only behaviour.
//
// Credentials are accepted as `Authorization: Bearer <key>` or `x-api-key`.
// The auth scheme is case-insensitive per RFC 9110 and comparison is
// constant-time. When both headers are present, a well-formed Bearer value
// wins; otherwise the x-api-key header is consulted.
func (s *Server) requireAPIKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.apiKey == "" || !strings.HasPrefix(r.URL.Path, "/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		provided := ""
		if h := r.Header.Get("Authorization"); h != "" {
			scheme, rest, ok := strings.Cut(h, " ")
			if ok && strings.EqualFold(scheme, "Bearer") {
				provided = strings.TrimLeft(rest, " ")
			}
		}
		if provided == "" {
			provided = r.Header.Get("x-api-key")
		}
		if subtle.ConstantTimeCompare([]byte(provided), []byte(s.apiKey)) != 1 {
			writeError(w, http.StatusUnauthorized,
				"invalid API key", "invalid_request_error")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

// modelObject is the OpenAI model shape plus the capability fields WorkBuddy
// advertises. Strict OpenAI clients ignore unknown keys.
type modelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`

	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Vendor      string `json:"vendor,omitempty"`

	MaxInputTokens  int `json:"max_input_tokens,omitempty"`
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`

	SupportsImages    bool `json:"supports_images,omitempty"`
	SupportsToolCall  bool `json:"supports_tool_calls,omitempty"`
	SupportsReasoning bool `json:"supports_reasoning,omitempty"`

	Credits string `json:"credits,omitempty"`
}

func toModelObject(m CachedModel, now int64) modelObject {
	return modelObject{
		ID:                m.ID,
		Object:            "model",
		Created:           now,
		OwnedBy:           "workbuddy",
		Name:              m.Name,
		Description:       m.Description,
		Vendor:            m.Vendor,
		MaxInputTokens:    m.MaxInputTokens,
		MaxOutputTokens:   m.MaxOutputTokens,
		SupportsImages:    m.SupportsImages,
		SupportsToolCall:  m.SupportsToolCall,
		SupportsReasoning: m.SupportsReasoning,
		Credits:           m.Credits,
	}
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	// GET /v1/models/<id> — a single model, or an OpenAI-shaped 404.
	if id := trimModelID(r.URL.Path); id != "" {
		s.handleModelByID(w, id)
		return
	}
	models, err := GetModels(s.auth)
	if err != nil {
		writeError(w, http.StatusBadGateway, "fetch models: "+err.Error(), "server_error")
		return
	}
	now := time.Now().Unix()
	list := make([]modelObject, 0, len(models))
	for _, m := range models {
		list = append(list, toModelObject(m, now))
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": list})
}

// trimModelID returns the <id> segment of /v1/models/<id>, or "" when the path
// addresses the collection itself. An id containing "/" is not a model id.
func trimModelID(path string) string {
	const prefix = "/v1/models/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	id := strings.TrimPrefix(path, prefix)
	if id == "" || strings.Contains(id, "/") {
		return ""
	}
	return id
}

// handleModelByID serves GET /v1/models/<id>.
func (s *Server) handleModelByID(w http.ResponseWriter, id string) {
	models, err := GetModels(s.auth)
	if err != nil {
		writeError(w, http.StatusBadGateway, "fetch models: "+err.Error(), "server_error")
		return
	}
	for _, m := range models {
		if m.ID == id {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(toModelObject(m, time.Now().Unix()))
			return
		}
	}
	writeError(w, http.StatusNotFound, "model not found: "+id, "invalid_request_error")
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

	cid := newCorrelationID()
	started := time.Now()
	reqModel, _, msgCount, lastUser := summariseRequest(body)
	logLine(cid, "▶ REQUEST %s | stream=%v | msgs=%d | last_user=%q",
		reqModel, clientWantsStream, msgCount, lastUser)
	logBlock(cid, "REQUEST BODY", string(body))

	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	payload["stream"] = true

	// Desensitize mode: rewrite moderation-triggering keywords in the system
	// prompt so upstream safety review does not reject otherwise-fine requests.
	// Rewrites are word-boundary-aware and cover every table entry, so no
	// separate pre-check is needed. Role is matched case-insensitively to
	// mirror UpstreamClient.Chat.
	if s.desensitize {
		if msgs, ok := payload["messages"].([]any); ok && len(msgs) > 0 {
			if first, ok := msgs[0].(map[string]any); ok {
				if role, ok := first["role"].(string); ok && strings.EqualFold(strings.TrimSpace(role), "system") {
					if content, ok := first["content"].(string); ok {
						if rewritten, changed := desensitizePrompt(content); changed {
							first["content"] = rewritten
						}
					}
				}
			}
		}
	}
	body, _ = json.Marshal(payload)

	resp, err := s.client.Chat(body)
	if err != nil {
		logLine(cid, "✗ UPSTREAM ERROR %s | %s", reqModel, err)
		writeError(w, http.StatusBadGateway, "upstream: "+err.Error(), "server_error")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Read the full error body so upstream messages are never truncated.
		errBody, _ := io.ReadAll(resp.Body)
		logLine(cid, "✗ UPSTREAM HTTP %d | %s", resp.StatusCode, redact(string(errBody)))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		w.Write(errBody)
		return
	}

	if clientWantsStream {
		copySSELogged(w, resp, cid, reqModel, started)
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
	// A mid-stream read failure must not be reported as a completed response.
	if err := scanner.Err(); err != nil {
		logLine(cid, "✗ UPSTREAM STREAM ERROR %s | %s", reqModel, err)
		writeError(w, http.StatusBadGateway, "upstream stream: "+err.Error(), "server_error")
		return
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
	chars := 0
	for _, acc := range accByIndex {
		for _, s := range acc.collected {
			chars += len(s)
		}
	}
	logLine(cid, "◀ RESPONSE %s | %.1fs | finish=%s | chars=%d",
		reqModel, time.Since(started).Seconds(), choices[0]["finish_reason"], chars)
	w.Header().Set("Content-Type", "application/json")
	respPayload := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"model":   model,
		"choices": choices,
	}
	if out, err := json.Marshal(respPayload); err == nil {
		logBlock(cid, "RESPONSE BODY", string(out))
	}
	json.NewEncoder(w).Encode(respPayload)
}

// copySSELogged streams SSE to the client while capturing the raw upstream
// stream for the trace file. Parses complete SSE data lines one at a time so
// that a finish_reason split across two reads is not silently dropped.
func copySSELogged(dst http.ResponseWriter, src *http.Response, cid, reqModel string, started time.Time) {
	dst.Header().Set("Content-Type", "text/event-stream")
	dst.Header().Set("Cache-Control", "no-cache")
	dst.Header().Set("Connection", "keep-alive")
	flusher, _ := dst.(http.Flusher)

	var raw strings.Builder
	finish := "stop"
	chars := 0
	scanner := bufio.NewScanner(src.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for scanner.Scan() {
		line := scanner.Text()
		// Emit original line (without trailing newline, since Scan strips it).
		dst.Write([]byte(line + "\n"))
		if flusher != nil {
			flusher.Flush()
		}
		if LoggingEnabled() {
			raw.WriteString(line + "\n")
			chars += len(line) + 1
			finish = finishReasonFromLine(line, finish)
		}
	}
	if err := scanner.Err(); err != nil {
		logLine(cid, "✗ UPSTREAM STREAM ERROR %s | %s", reqModel, err)
	}
	if LoggingEnabled() {
		logLine(cid, "◀ RESPONSE %s | %.1fs | finish=%s | chars=%d (streamed)",
			reqModel, time.Since(started).Seconds(), finish, chars)
		logBlock(cid, "RESPONSE RAW SSE", raw.String())
	}
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
