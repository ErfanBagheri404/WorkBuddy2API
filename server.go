package main

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"sort"
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
	mux.HandleFunc("/v1/messages", s.handleAnthropic)
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
	Free    bool   `json:"free,omitempty"`

	OnlyReasoning      bool `json:"only_reasoning,omitempty"`
	CanDisableThinking bool `json:"can_disable_thinking,omitempty"`
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
		SupportsImages:     m.SupportsImages,
		SupportsToolCall:   m.SupportsToolCall,
		SupportsReasoning:  m.SupportsReasoning,
		Credits:            m.Credits,
		Free:               m.Free,
		OnlyReasoning:      m.OnlyReasoning,
		CanDisableThinking: m.CanDisableThinking,
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
	models = FilterUsableModels(s.auth.UID(), models)
	MaybeProbeModels(s.client, s.auth.UID(), models)
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
	models = FilterUsableModels(s.auth.UID(), models)
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

	resp, err := s.client.Chat(r.Context(), body)
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
		if verdict := ClassifyModelError(resp.StatusCode, errBody); verdict != "" {
			RecordModelVerdict(s.auth.UID(), reqModel, verdict)
		}
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
	sawDone := false
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for scanner.Scan() {
		line := scanner.Text()
		data, ok := sseData(line)
		if !ok {
			continue
		}
		if data == "[DONE]" {
			sawDone = true
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
	// The upstream SSE must end with a terminal finish_reason (and, in
	// practice, [DONE]); otherwise the content is a truncated generation and
	// returning it as a completed JSON response would silently lose output.
	unterminated := make([]int, 0, len(accByIndex))
	for idx, acc := range accByIndex {
		if acc.finish == "" {
			unterminated = append(unterminated, idx)
		}
	}
	if len(unterminated) > 0 || !sawDone {
		sort.Ints(unterminated)
		logLine(cid, "✗ UPSTREAM STREAM INCOMPLETE %s | unterminated choices=%v | done=%v",
			reqModel, unterminated, sawDone)
		writeError(w, http.StatusBadGateway, "upstream stream ended without finish_reason", "server_error")
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

// sseData extracts the payload of an SSE data line, accepting the spec's
// optional whitespace after the colon. ok is false for non-data lines.
func sseData(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(line, "data:")), true
}

func isDoneLine(line string) bool {
	data, ok := sseData(line)
	return ok && data == "[DONE]"
}

// normalizeIntermediateFinishReason rewrites the non-standard
// `"finish_reason": ""` that the upstream gateway puts on every intermediate
// chunk to `null`, which is what the OpenAI schema specifies. Parsing the line
// as JSON (rather than replacing substrings) avoids corrupting chunk content
// that happens to contain the same text, and covers any JSON whitespace layout.
func normalizeIntermediateFinishReason(line string) string {
	data, ok := sseData(line)
	if !ok || data == "[DONE]" || !strings.Contains(data, `"finish_reason"`) {
		return line
	}
	var chunk struct {
		Choices []struct {
			FinishReason json.RawMessage `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal([]byte(data), &chunk) != nil {
		return line
	}
	changed := false
	for i := range chunk.Choices {
		if string(chunk.Choices[i].FinishReason) == `""` {
			chunk.Choices[i].FinishReason = json.RawMessage(`null`)
			changed = true
		}
	}
	if !changed {
		return line
	}
	// Decode into a generic map so unknown upstream fields survive, then
	// replace only the finish_reason values.
	var generic map[string]json.RawMessage
	if json.Unmarshal([]byte(data), &generic) != nil {
		return line
	}
	var choices []map[string]json.RawMessage
	if json.Unmarshal(generic["choices"], &choices) != nil {
		return line
	}
	for i := range choices {
		if string(choices[i]["finish_reason"]) == `""` {
			choices[i]["finish_reason"] = json.RawMessage(`null`)
		}
	}
	normalizedChoices, err := json.Marshal(choices)
	if err != nil {
		return line
	}
	generic["choices"] = normalizedChoices
	payload, err := json.Marshal(generic)
	if err != nil {
		return line
	}
	return "data: " + string(payload)
}

// sseChunkMeta carries the identity of the upstream completion so a synthesized
// terminal chunk belongs to the same completion as the chunks before it.
type sseChunkMeta struct {
	id    string
	model string
}

func chunkMetaFromLine(line string, meta sseChunkMeta) sseChunkMeta {
	data, ok := sseData(line)
	if !ok || data == "[DONE]" {
		return meta
	}
	var c struct {
		ID    string `json:"id"`
		Model string `json:"model"`
	}
	if json.Unmarshal([]byte(data), &c) != nil {
		return meta
	}
	if c.ID != "" {
		meta.id = c.ID
	}
	if c.Model != "" {
		meta.model = c.Model
	}
	return meta
}

// writeTerminalChunks emits one final OpenAI chunk per choice index that has
// not yet reported a finish_reason, then leaves [DONE] to the caller. It is
// used when the upstream stream ends without terminating every choice: an SSE
// body that simply stops is invalid output for OpenAI-compatible clients.
func writeTerminalChunks(dst io.Writer, flusher http.Flusher, meta sseChunkMeta, pending []int) {
	id := meta.id
	if id == "" {
		id = "chatcmpl-" + newRequestID()
	}
	model := meta.model
	choices := make([]any, 0, len(pending))
	for _, idx := range pending {
		choices = append(choices, map[string]any{
			"index":         idx,
			"delta":         map[string]any{},
			"finish_reason": "length",
		})
	}
	chunk := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": choices,
	}
	payload, err := json.Marshal(chunk)
	if err != nil {
		return
	}
	// Leading blank line closes any unterminated upstream event.
	dst.Write([]byte("\ndata: " + string(payload) + "\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
}

// writeDONE terminates the response with the [DONE] sentinel.
func writeDONE(dst io.Writer, flusher http.Flusher) {
	dst.Write([]byte("data: [DONE]\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
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
	seenChoices := map[int]bool{}
	terminated := map[int]bool{}
	meta := sseChunkMeta{model: reqModel}
	sawDone := false
	writeFailed := false
	clientGone := false

	// A disconnect does not reliably fail the next write (the response may be
	// buffered, and proxies can retry), so poll the request context explicitly.
	// Without this the handler keeps draining a generation nobody is reading.
	var reqCtx context.Context
	if src.Request != nil {
		reqCtx = src.Request.Context()
	}

	scanner := bufio.NewScanner(src.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	writeLine := func(line string) bool {
		// Emit original line (without trailing newline, since Scan strips it).
		if _, err := dst.Write([]byte(line + "\n")); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		if LoggingEnabled() {
			raw.WriteString(line + "\n")
			chars += len(line) + 1
			finish = finishReasonFromLine(line, finish)
		}
		return true
	}
	for scanner.Scan() {
		if reqCtx != nil && reqCtx.Err() != nil {
			clientGone = true
			break
		}
		line := scanner.Text()
		if isDoneLine(line) {
			if pending := pendingChoices(seenChoices, terminated); len(pending) > 0 {
				writeTerminalChunks(dst, flusher, meta, pending)
			}
			if !writeLine(line) {
				writeFailed = true
				break
			}
			sawDone = true
			// Anything after [DONE] belongs to no event the client will read.
			break
		}
		meta = chunkMetaFromLine(line, meta)
		for idx := range choiceIndexes(line) {
			seenChoices[idx] = true
		}
		for idx := range choiceFinishReasons(line) {
			terminated[idx] = true
		}
		if !writeLine(normalizeIntermediateFinishReason(line)) {
			writeFailed = true
			break
		}
	}
	if err := scanner.Err(); err != nil {
		logLine(cid, "✗ UPSTREAM STREAM ERROR %s | %s", reqModel, err)
	}
	if writeFailed {
		logLine(cid, "✗ DOWNSTREAM WRITE FAILED %s | client disconnected", reqModel)
	} else if clientGone {
		logLine(cid, "✗ CLIENT DISCONNECTED %s | stopped reading upstream", reqModel)
	} else {
		// Never let the response end on a bare EOF: synthesize terminal chunks
		// and the sentinel the client needs to treat the turn as complete.
		// Only finish_reason is "length": that is what a truncated generation is.
		if pending := pendingChoices(seenChoices, terminated); len(pending) > 0 {
			logLine(cid, "⚠ SYNTHESIZED TERMINAL %s | choices=%v | upstream ended without finish_reason",
				reqModel, pending)
			writeTerminalChunks(dst, flusher, meta, pending)
		}
		if !sawDone {
			writeDONE(dst, flusher)
		}
	}
	if LoggingEnabled() {
		logLine(cid, "◀ RESPONSE %s | %.1fs | finish=%s | chars=%d (streamed)",
			reqModel, time.Since(started).Seconds(), finish, chars)
		logBlock(cid, "RESPONSE RAW SSE", raw.String())
	}
}

// choiceIndexes returns every choice index present on an SSE data line, even
// when the chunk carries no terminal finish_reason (intermediate deltas).
func choiceIndexes(line string) map[int]bool {
	data, ok := sseData(line)
	if !ok || data == "[DONE]" {
		return nil
	}
	var c struct {
		Choices []struct {
			Index int `json:"index"`
		} `json:"choices"`
	}
	if json.Unmarshal([]byte(data), &c) != nil {
		return nil
	}
	out := make(map[int]bool, len(c.Choices))
	for _, ch := range c.Choices {
		out[ch.Index] = true
	}
	return out
}

// pendingChoices returns the sorted choice indexes that were seen but never
// terminated. An empty stream still needs a terminal for choice 0.
func pendingChoices(seen, terminated map[int]bool) []int {
	if len(seen) == 0 {
		if terminated[0] {
			return nil
		}
		return []int{0}
	}
	var pending []int
	for idx := range seen {
		if !terminated[idx] {
			pending = append(pending, idx)
		}
	}
	sort.Ints(pending)
	return pending
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

// handleAnthropic serves POST /v1/messages (Anthropic Messages API).
// Translates to upstream OpenAI chat, translates the response back.
func (s *Server) handleAnthropic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 10<<20))
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "read body: "+err.Error(), "invalid_request_error")
		return
	}
	var ar anthropicRequest
	if err := json.Unmarshal(body, &ar); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "invalid_request_error")
		return
	}
	if strings.TrimSpace(ar.Model) == "" {
		writeAnthropicError(w, http.StatusBadRequest, "model is required", "invalid_request_error")
		return
	}
	if ar.MaxTokens <= 0 {
		ar.MaxTokens = 4096
	}

	// Convert Anthropic request → OpenAI payload
	payload := anthropicToOpenAI(&ar)

	// Desensitize system prompt if enabled
	if s.desensitize {
		if msgs, ok := payload["messages"].([]any); ok && len(msgs) > 0 {
			if first, ok := msgs[0].(map[string]any); ok {
				if role, ok := first["role"].(string); ok && strings.EqualFold(role, "system") {
					if content, ok := first["content"].(string); ok {
						if rewritten, changed := desensitizePrompt(content); changed {
							first["content"] = rewritten
						}
					}
				}
			}
		}
	}

	openaiBody, _ := json.Marshal(payload)

	cid := newCorrelationID()
	started := time.Now()
	logLine(cid, "▶ ANTHROPIC %s | max_tokens=%d | stream=%v",
		ar.Model, ar.MaxTokens, ar.Stream == nil || *ar.Stream)

	resp, err := s.client.Chat(r.Context(), openaiBody)
	if err != nil {
		logLine(cid, "✗ UPSTREAM ERROR %s | %s", ar.Model, err)
		writeAnthropicError(w, http.StatusBadGateway, "upstream: "+err.Error(), "api_error")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(resp.Body)
		logLine(cid, "✗ UPSTREAM HTTP %d", resp.StatusCode)
		if verdict := ClassifyModelError(resp.StatusCode, errBody); verdict != "" {
			RecordModelVerdict(s.auth.UID(), ar.Model, verdict)
		}
		writeAnthropicError(w, resp.StatusCode, string(errBody), "api_error")
		return
	}

	if ar.Stream == nil || *ar.Stream {
		anthropicStream(w, resp, &ar, cid)
	} else {
		anthropicNonStream(w, resp, &ar, cid)
	}
	logLine(cid, "◀ ANTHROPIC %s | %.1fs", ar.Model, time.Since(started).Seconds())
}
