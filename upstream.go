package main

import (
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
}

// ModelCache fetches and caches the model list from WorkBuddy.
type ModelCache struct {
	mu      sync.RWMutex
	models  []CachedModel
	fetched time.Time
}

var globalModelCache = &ModelCache{}

// extraModels are direct model names accepted by /v2/chat/completions
// but NOT advertised in /enterprises/personal/models.
var extraModels = []CachedModel{
	{ID: "deepseek-v4.1-flash", Name: "DeepSeek-V4.1-Flash"},
	{ID: "deepseek-v3", Name: "DeepSeek-V3"},
}

// FetchModels hits the upstream /v2/enterprises/personal/models endpoint
// and appends the extra known direct names.
func FetchModels(am *AuthManager) ([]CachedModel, error) {
	resp, err := am.doUpstream(http.MethodGet, "/v2/enterprises/personal/models", nil, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch models: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read models response: %w", err)
	}
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
	// Merge extra names that aren't already present.
	existing := make(map[string]bool, len(models))
	for _, m := range models {
		existing[m.ID] = true
	}
	for _, em := range extraModels {
		if !existing[em.ID] {
			models = append(models, em)
		}
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("no models returned")
	}
	globalModelCache.mu.Lock()
	globalModelCache.models = models
	globalModelCache.fetched = time.Now()
	globalModelCache.mu.Unlock()
	return models, nil
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

// UpstreamClient forwards chat requests to WorkBuddy.
type UpstreamClient struct {
	auth *AuthManager
}

func NewUpstreamClient(am *AuthManager) *UpstreamClient {
	return &UpstreamClient{auth: am}
}

// Chat forwards an OpenAI chat request to /v2/chat/completions.
func (c *UpstreamClient) Chat(reqBody []byte) (*http.Response, error) {
	var payload map[string]any
	if err := json.Unmarshal(reqBody, &payload); err != nil {
		return nil, fmt.Errorf("invalid request JSON: %w", err)
	}

	// stream defaults to true
	if _, ok := payload["stream"]; !ok {
		payload["stream"] = true
	}

	// Ensure first message is system
	msgs, _ := payload["messages"].([]any)
	if len(msgs) == 0 {
		return nil, fmt.Errorf("messages must not be empty")
	}
	first, _ := msgs[0].(map[string]any)
	role, _ := first["role"].(string)
	if strings.ToLower(strings.TrimSpace(role)) != "system" {
		payload["messages"] = append([]any{map[string]any{
			"role":    "system",
			"content": "You are a helpful assistant.",
		}}, msgs...)
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	stream, _ := payload["stream"].(bool)
	accept := "application/json"
	if stream {
		accept = "text/event-stream, application/json"
	}
	resp, err := c.auth.doUpstream(http.MethodPost, "/v2/chat/completions", body,
		map[string]string{"Accept": accept})
	if err != nil {
		return nil, fmt.Errorf("upstream request: %w", err)
	}
	return resp, nil
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
