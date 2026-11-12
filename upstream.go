package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// CachedModel holds info from /v2/enterprises/personal/models. The capability
// and context-window fields are passed through to /v1/models so clients can
// render badges and pick a model by window size.
type CachedModel struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Default     bool   `json:"isDefault"`
	Description string `json:"descriptionEn,omitempty"`
	Vendor      string `json:"vendor,omitempty"`

	MaxInputTokens  int `json:"maxInputTokens,omitempty"`
	MaxOutputTokens int `json:"maxOutputTokens,omitempty"`

	SupportsImages    bool `json:"supportsImages,omitempty"`
	SupportsToolCall  bool `json:"supportsToolCall,omitempty"`
	SupportsReasoning bool `json:"supportsReasoning,omitempty"`

	Credits string `json:"credits,omitempty"`
	Free    bool   `json:"free,omitempty"`

	// OnlyReasoning and CanDisableThinking come from the desktop app's
	// product.json (the cloud list omits them). They drive the max_tokens
	// floor in Chat: thinking models burn the whole budget on reasoning.
	OnlyReasoning      bool `json:"only_reasoning,omitempty"`
	CanDisableThinking bool `json:"can_disable_thinking,omitempty"`
}

// ModelCache fetches and caches the model list from WorkBuddy.
type ModelCache struct {
	mu      sync.RWMutex
	models  []CachedModel
	fetched time.Time
}

var globalModelCache = &ModelCache{}

// knownDirectModels are chat endpoints that work against
// /v2/chat/completions but belong to no catalog: direct model names accepted
// by the gateway. They stay a tiny allowlist because nothing dynamic lists
// them (product.json has "deepseek-v4.1-flash", which the dynamic merge picks
// up on its own). Probe results still decide whether each one stays visible
// for the current account.
var knownDirectModels = []CachedModel{
	{ID: "deepseek-v3", Name: "DeepSeek-V3", Credits: "x0.00 credits", Free: true},
}

// tryAccounts walks a fixed account order for one operation, retrying on
// quota/rate-limit responses and transport errors. It returns the response and
// the exact account that served it, so callers can attribute per-account state
// (model verdicts, probes) correctly. The order is local to this call: nothing
// here mutates a shared cursor between the attempt and the retry, which is what
// let concurrent requests collide on the same account before.
func tryAccounts(order []*AuthManager, call func(*AuthManager) (*http.Response, error)) (*http.Response, *AuthManager, error) {
	var lastErr error
	var lastResp *http.Response
	var lastAM *AuthManager
	for _, am := range order {
		resp, err := call(am)
		if err != nil {
			lastErr = err
			lastAM = am
			continue
		}
		// A 429/5xx can carry a quota marker in the body. Non-streaming
		// responses are read fully here so failover can inspect them; streaming
		// responses are returned immediately (SSE has already started).
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lastResp = newHTTPResponse(resp, errBody)
			lastAM = am
			if IsQuotaError(resp.StatusCode, string(errBody)) {
				continue
			}
			return lastResp, am, nil
		}
		return resp, am, nil
	}
	if lastResp != nil {
		return lastResp, lastAM, nil
	}
	if lastErr != nil {
		if len(order) == 1 {
			return nil, lastAM, lastErr
		}
		return nil, lastAM, fmt.Errorf("all accounts failed; last error: %w", lastErr)
	}
	return nil, lastAM, fmt.Errorf("no accounts available")
}

// accountOrder returns the accounts to try for one operation. A pool-backed
// client gets its own fresh rotation; a single-account client always returns
// its one account.
func (c *UpstreamClient) accountOrder() []*AuthManager {
	if c.pool != nil {
		return c.pool.Order()
	}
	return []*AuthManager{c.auth}
}

// parseModels builds the model list from a raw upstream response body,
// merging the installed desktop app's product.json and the known direct names.
func parseModels(raw []byte) ([]CachedModel, error) {
	var envelope struct {
		Code int `json:"code"`
		Data struct {
			Models []CachedModel `json:"models"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("decode models response: %w", err)
	}
	if envelope.Code != 0 {
		return nil, fmt.Errorf("models API error code=%d", envelope.Code)
	}
	models := envelope.Data.Models
	existing := make(map[string]bool, len(models))
	for _, m := range models {
		existing[m.ID] = true
	}
	byID, order, localPath := loadLocalModels()
	for _, id := range order {
		if !existing[id] {
			models = append(models, cachedModelFromLocal(id, byID[id]))
			existing[id] = true
		}
	}
	for _, em := range knownDirectModels {
		if !existing[em.ID] {
			models = append(models, em)
		}
	}
	for i := range models {
		if lm, ok := byID[models[i].ID]; ok {
			models[i] = enrichFromLocal(models[i], lm)
		}
		models[i].Free = models[i].Free || isFreeCredits(models[i].Credits)
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("no models returned")
	}
	if localPath != "" {
		fmt.Printf("model catalog: %d from upstream, merged local product.json: %s\n", len(envelope.Data.Models), localPath)
	}
	return models, nil
}

// FetchModels hits the upstream /v2/enterprises/personal/models endpoint
// and appends the extra known direct names.
func FetchModels(am *AuthManager) ([]CachedModel, error) {
	resp, err := am.doUpstream(context.Background(), http.MethodGet, "/v2/enterprises/personal/models", nil, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch models: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read models response: %w", err)
	}
	models, err := parseModels(raw)
	if err != nil {
		return nil, err
	}
	globalModelCache.mu.Lock()
	globalModelCache.models = models
	globalModelCache.fetched = time.Now()
	globalModelCache.mu.Unlock()
	return models, nil
}

// FetchModelsFor fetches the model list using the client's account pool when
// one is present, failing over to the next account on a quota response, and
// returns the account that supplied the list. Parsing happens only after
// failover selects the successful response, and the shared cache is updated
// once, with the winning account's data.
func FetchModelsFor(c *UpstreamClient) ([]CachedModel, *AuthManager, error) {
	resp, am, err := tryAccounts(c.accountOrder(), func(am *AuthManager) (*http.Response, error) {
		return am.doUpstream(context.Background(), http.MethodGet, "/v2/enterprises/personal/models", nil, nil)
	})
	if err != nil {
		return nil, am, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, am, fmt.Errorf("read models response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, am, fmt.Errorf("models HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	models, err := parseModels(raw)
	if err != nil {
		return nil, am, err
	}
	globalModelCache.mu.Lock()
	globalModelCache.models = models
	globalModelCache.fetched = time.Now()
	globalModelCache.mu.Unlock()
	return models, am, nil
}

// GetModels returns cached models or fetches if stale.
func GetModels(am *AuthManager) ([]CachedModel, error) {
	globalModelCache.mu.RLock()
	if len(globalModelCache.models) > 0 && time.Since(globalModelCache.fetched) < 10*time.Minute {
		models := globalModelCache.models
		globalModelCache.mu.RUnlock()
		return models, nil
	}
	globalModelCache.mu.RUnlock()
	return FetchModels(am)
}

// GetModelsFor returns cached models (or fetches them via the pool) together
// with the account that supplied them. A cached list is attributed to the pool's
// current primary account; a fresh fetch uses the account that actually
// answered, which is the one any model verdicts belong to.
func (c *UpstreamClient) GetModelsFor() ([]CachedModel, *AuthManager, error) {
	globalModelCache.mu.RLock()
	if len(globalModelCache.models) > 0 && time.Since(globalModelCache.fetched) < 10*time.Minute {
		models := globalModelCache.models
		globalModelCache.mu.RUnlock()
		return models, c.currentAuth(), nil
	}
	globalModelCache.mu.RUnlock()
	return FetchModelsFor(c)
}

// ModelIDs returns just the ID strings.
func ModelIDs(models []CachedModel) []string {
	ids := make([]string, len(models))
	for i, m := range models {
		ids[i] = m.ID
	}
	return ids
}

// DefaultModel returns the default model ID, or the first one.
func DefaultModel(models []CachedModel) string {
	for _, m := range models {
		if m.Default {
			return m.ID
		}
	}
	if len(models) > 0 {
		return models[0].ID
	}
	return "default-model"
}

// UpstreamClient forwards chat requests to WorkBuddy. When an AccountPool is
// set, it transparently retries on 429/quota errors using the next account.
type UpstreamClient struct {
	auth *AuthManager
	pool *AccountPool
}

func NewUpstreamClient(am *AuthManager) *UpstreamClient {
	return &UpstreamClient{auth: am}
}

// NewUpstreamClientPool creates a client backed by an account pool for
// round-robin selection and failover. No single account is pinned into the
// client: each operation gets its own rotation of the pool.
func NewUpstreamClientPool(pool *AccountPool) *UpstreamClient {
	return &UpstreamClient{pool: pool}
}

// Chat forwards an OpenAI chat request to /v2/chat/completions. It returns the
// response and the account that served it, so the handler can attribute model
// verdicts to the right account when a pool is in use.
func (c *UpstreamClient) Chat(ctx context.Context, reqBody []byte) (*http.Response, *AuthManager, error) {
	return c.chatWithOptions(ctx, reqBody, chatOptions{})
}

// chatOptions tunes what Chat rewrites before forwarding.
type chatOptions struct {
	// skipReasoningFloor leaves max_tokens untouched (model probing).
	skipReasoningFloor bool
}

// chatWithOptions forwards an OpenAI chat request to /v2/chat/completions.
// ctx is the incoming client request's context: when the caller disconnects,
// the upstream request is cancelled so generation stops instead of running on.
// It returns the response and the account that served it.
func (c *UpstreamClient) chatWithOptions(ctx context.Context, reqBody []byte, opts chatOptions) (*http.Response, *AuthManager, error) {
	var payload map[string]any
	if err := json.Unmarshal(reqBody, &payload); err != nil {
		return nil, nil, fmt.Errorf("invalid request JSON: %w", err)
	}

	// stream defaults to true
	if _, ok := payload["stream"]; !ok {
		payload["stream"] = true
	}

	// Ensure first message is system
	msgs, _ := payload["messages"].([]any)
	if len(msgs) == 0 {
		return nil, nil, fmt.Errorf("messages must not be empty")
	}
	first, _ := msgs[0].(map[string]any)
	role, _ := first["role"].(string)
	if strings.ToLower(strings.TrimSpace(role)) != "system" {
		payload["messages"] = append([]any{map[string]any{
			"role":    "system",
			"content": "You are a helpful assistant.",
		}}, msgs...)
	}

	// Thinking models spend reasoning tokens out of the same max_tokens
	// budget: a small budget produces reasoning_content but no answer. Give
	// them a floor that can hold a short reasoning pass plus the answer.
	if !opts.skipReasoningFloor {
		if floor := reasoningTokenFloor(payload); floor > 0 {
			payload["max_tokens"] = floor
		}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, err
	}

	stream, _ := payload["stream"].(bool)
	accept := "application/json"
	if stream {
		accept = "text/event-stream, application/json"
	}

	// Upstream channel verification (code 11128, since 2026-10-15): the
	// gateway rejects chat requests that don't carry the X-Conversation-*
	// header family the official IDE sends on every turn. Derive a stable
	// conversation id from the session key (or the opening user message) and
	// mint fresh per-request / per-message UUIDs.
	sessionKey := ""
	for _, k := range []string{"session_id", "prompt_cache_key", "conversation_id"} {
		if v, ok := payload[k].(string); ok && v != "" {
			sessionKey = v
			break
		}
	}
	userText := ""
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := mm["role"].(string)
		if !strings.EqualFold(strings.TrimSpace(role), "user") {
			continue
		}
		switch v := mm["content"].(type) {
		case string:
			userText = v
		case []any:
			var parts []string
			for _, b := range v {
				bm, ok := b.(map[string]any)
				if !ok {
					continue
				}
				if t, _ := bm["text"].(string); t != "" {
					parts = append(parts, t)
				}
			}
			userText = strings.Join(parts, "\n")
		}
		break
	}
	conversationKey := sessionKey
	if conversationKey == "" && userText != "" {
		conversationKey = "cbai:" + userText
	}
	convID := randomUUID()
	if conversationKey != "" {
		convID = stableUUID(conversationKey)
	}
	reqID := randomUUID()
	msgID := randomUUID()

	extraHeaders := map[string]string{
		"Accept":                       accept,
		"X-Conversation-ID":            convID,
		"X-Conversation-Request-ID":     reqID,
		"X-Conversation-Message-ID":     msgID,
		"X-Request-ID":                 msgID,
	}

	resp, am, err := c.doWithFailover(ctx, http.MethodPost, "/v2/chat/completions", body, extraHeaders)
	if err != nil {
		return nil, am, fmt.Errorf("upstream request: %w", err)
	}
	return resp, am, nil
}

// doWithFailover posts to upstream. It walks a per-operation rotation of the
// pool (or the single account), retrying on quota/rate-limit responses and
// transport errors, and returns the account that produced the final result.
func (c *UpstreamClient) doWithFailover(ctx context.Context, method, path string, body []byte, headers map[string]string) (*http.Response, *AuthManager, error) {
	return tryAccounts(c.accountOrder(), func(am *AuthManager) (*http.Response, error) {
		return am.doUpstream(ctx, method, path, body, headers)
	})
}

// currentAuth returns the pool's current primary account (or the single
// account). Used only to attribute cached state; request failover uses
// accountOrder so it never depends on this shared cursor.
func (c *UpstreamClient) currentAuth() *AuthManager {
	if c.pool != nil {
		return c.pool.Peek()
	}
	return c.auth
}

// newHTTPResponse rebuilds an *http.Response with the body already read,
// so the caller can read it without the original (closed) body.
func newHTTPResponse(orig *http.Response, body []byte) *http.Response {
	clone := *orig
	clone.Body = io.NopCloser(bytes.NewReader(body))
	return &clone
}

func copySSE(dst http.ResponseWriter, src *http.Response) {
	dst.Header().Set("Content-Type", "text/event-stream")
	dst.Header().Set("Cache-Control", "no-cache")
	dst.Header().Set("Connection", "keep-alive")
	flusher, _ := dst.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Body.Read(buf)
		if n > 0 {
			dst.Write(buf[:n])
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func copyBody(w http.ResponseWriter, resp *http.Response, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	io.Copy(w, resp.Body)
}
