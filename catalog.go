package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// The WorkBuddy desktop app ships the authoritative model catalog inside
// resources/app.asar.unpacked/cli/product.json. The cloud endpoint
// /v2/enterprises/personal/models lags behind it: models the app already
// offers (typically the newly added free ones) are missing there until the
// backend rolls them out to every product config.
//
// So the advertised model list is built from both sources: the cloud list plus
// the chat models found in the installed app's product.json. Nothing is
// hardcoded, so a WorkBuddy app update that ships new models shows up in
// /v1/models without a new proxy release.
//
// Override the discovery with WORKBUDDY2API_PRODUCT_JSON=/path/to/product.json.

const productJSONEnv = "WORKBUDDY2API_PRODUCT_JSON"

// localModel is one entry of product.json "models". Only the fields that
// matter for an OpenAI-shaped listing are decoded.
type localModel struct {
	ID                string          `json:"id"`
	Name              string          `json:"name"`
	Credits           string          `json:"credits"`
	Vendor            string          `json:"vendor"`
	DescriptionEn     string          `json:"descriptionEn"`
	MaxInputTokens    int             `json:"maxInputTokens"`
	MaxOutputTokens   int             `json:"maxOutputTokens"`
	SupportsImages    bool            `json:"supportsImages"`
	SupportsToolCall  bool            `json:"supportsToolCall"`
	SupportsReasoning bool            `json:"supportsReasoning"`
	OnlyReasoning     bool            `json:"onlyReasoning"`
	CanDisableThinking bool           `json:"canDisableThinking"`
	Tags              []string        `json:"tags"`
	VideoGeneration   json.RawMessage `json:"videoGeneration"`
	ImageGeneration   json.RawMessage `json:"imageGeneration"`
	Reasoning         *struct {
		CanDisableThinking bool `json:"canDisableThinking"`
	} `json:"reasoning"`
}

// chatCapable reports whether the entry can be used through
// /v1/chat/completions. Image and video generators sit in the same "models"
// array but carry no credit price and are only reachable from their own
// endpoints, so they must not be advertised as chat models.
func (m localModel) chatCapable() bool {
	if strings.TrimSpace(m.ID) == "" {
		return false
	}
	if strings.TrimSpace(m.Credits) == "" {
		return false
	}
	if len(m.VideoGeneration) > 0 || len(m.ImageGeneration) > 0 {
		return false
	}
	for _, tag := range m.Tags {
		switch strings.ToLower(strings.TrimSpace(tag)) {
		case "text-to-image", "image-to-image", "text-to-video", "image-to-video":
			return false
		}
	}
	return true
}

// canDisableThinking merges the two places product.json records the flag.
func (m localModel) canDisableThinking() bool {
	if m.CanDisableThinking {
		return true
	}
	return m.Reasoning != nil && m.Reasoning.CanDisableThinking
}

// isFreeCredits reports whether a "x0.00 credits" price means free usage.
func isFreeCredits(credits string) bool {
	v := strings.ToLower(strings.TrimSpace(credits))
	v = strings.TrimPrefix(v, "x")
	v = strings.TrimSpace(strings.TrimSuffix(v, "credits"))
	if v == "" {
		return false
	}
	f, err := strconv.ParseFloat(v, 64)
	return err == nil && f == 0
}

var localCatalog struct {
	mu    sync.Mutex
	stamp string
	path  string
	order []string
	byID  map[string]localModel
}

// productJSONCandidates lists the install locations of the desktop app, most
// specific first. Any directory whose name contains "workbuddy" under the
// usual program roots is probed as well, so renamed or side-by-side installs
// keep working.
func productJSONCandidates() []string {
	var out []string
	if v := strings.TrimSpace(os.Getenv(productJSONEnv)); v != "" {
		out = append(out, v)
	}
	rel := filepath.Join("resources", "app.asar.unpacked", "cli", "product.json")

	var roots []string
	if la := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); la != "" {
		roots = append(roots, filepath.Join(la, "Programs"))
	}
	for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "ProgramW6432", "ProgramData"} {
		if d := strings.TrimSpace(os.Getenv(env)); d != "" {
			roots = append(roots, d)
		}
	}
	for _, root := range roots {
		out = append(out, filepath.Join(root, "WorkBuddyAI", rel))
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || !strings.Contains(strings.ToLower(e.Name()), "workbuddy") {
				continue
			}
			out = append(out, filepath.Join(root, e.Name(), rel))
		}
	}
	out = append(out,
		"/Applications/WorkBuddyAI.app/Contents/Resources/app.asar.unpacked/cli/product.json",
		"/opt/WorkBuddyAI/resources/app.asar.unpacked/cli/product.json",
		"/usr/share/workbuddyai/resources/app.asar.unpacked/cli/product.json",
	)
	return out
}

// discoverProductJSON returns the newest existing product.json, or "" when the
// desktop app is not installed.
func discoverProductJSON() string {
	best := ""
	var bestMod int64
	for _, cand := range productJSONCandidates() {
		fi, err := os.Stat(cand)
		if err != nil || fi.IsDir() {
			continue
		}
		if mod := fi.ModTime().UnixNano(); mod > bestMod {
			best, bestMod = cand, mod
		}
	}
	return best
}

// LocalCatalogPath exposes the discovered catalog path for diagnostics.
func LocalCatalogPath() string {
	_, _, path := loadLocalModels()
	return path
}

// loadLocalModels parses product.json, re-reading it only when the file
// changed. Returns the chat models by id, their product.json order, and the
// file it came from.
func loadLocalModels() (map[string]localModel, []string, string) {
	path := discoverProductJSON()
	if path == "" {
		return nil, nil, ""
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, nil, ""
	}
	stamp := fmt.Sprintf("%s|%d|%d", path, fi.ModTime().UnixNano(), fi.Size())

	localCatalog.mu.Lock()
	defer localCatalog.mu.Unlock()
	if localCatalog.stamp == stamp {
		return localCatalog.byID, localCatalog.order, localCatalog.path
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return localCatalog.byID, localCatalog.order, localCatalog.path
	}
	var doc struct {
		Models []localModel `json:"models"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return localCatalog.byID, localCatalog.order, localCatalog.path
	}

	byID := make(map[string]localModel, len(doc.Models))
	order := make([]string, 0, len(doc.Models))
	for _, m := range doc.Models {
		if !m.chatCapable() {
			continue
		}
		if _, dup := byID[m.ID]; dup {
			continue
		}
		byID[m.ID] = m
		order = append(order, m.ID)
	}

	localCatalog.stamp = stamp
	localCatalog.path = path
	localCatalog.byID = byID
	localCatalog.order = order
	return byID, order, path
}

// enrichFromLocal fills capability metadata the cloud list omits. Cloud values
// win when both sides carry a field.
func enrichFromLocal(m CachedModel, lm localModel) CachedModel {
	if m.Name == "" {
		m.Name = lm.Name
	}
	if m.Description == "" {
		m.Description = lm.DescriptionEn
	}
	if m.Vendor == "" {
		m.Vendor = lm.Vendor
	}
	if m.MaxInputTokens == 0 {
		m.MaxInputTokens = lm.MaxInputTokens
	}
	if m.MaxOutputTokens == 0 {
		m.MaxOutputTokens = lm.MaxOutputTokens
	}
	m.SupportsImages = m.SupportsImages || lm.SupportsImages
	m.SupportsToolCall = m.SupportsToolCall || lm.SupportsToolCall
	m.SupportsReasoning = m.SupportsReasoning || lm.SupportsReasoning
	m.OnlyReasoning = m.OnlyReasoning || lm.OnlyReasoning
	m.CanDisableThinking = m.CanDisableThinking || lm.canDisableThinking()
	if m.Credits == "" {
		m.Credits = lm.Credits
	}
	m.Free = m.Free || isFreeCredits(m.Credits)
	return m
}

func cachedModelFromLocal(id string, lm localModel) CachedModel {
	return CachedModel{
		ID:                 id,
		Name:               lm.Name,
		Description:        lm.DescriptionEn,
		Vendor:             lm.Vendor,
		MaxInputTokens:     lm.MaxInputTokens,
		MaxOutputTokens:    lm.MaxOutputTokens,
		SupportsImages:     lm.SupportsImages,
		SupportsToolCall:   lm.SupportsToolCall,
		SupportsReasoning:  lm.SupportsReasoning,
		OnlyReasoning:      lm.OnlyReasoning,
		CanDisableThinking: lm.canDisableThinking(),
		Credits:            lm.Credits,
		Free:               isFreeCredits(lm.Credits),
	}
}

// mergeLocalCatalog appends every chat model the installed app knows about but
// the cloud list does not, and enriches the shared ones. Cloud order is kept
// (it drives the client's default pick); app-only models follow in
// product.json order so the output is deterministic.
func mergeLocalCatalog(cloud []CachedModel) []CachedModel {
	byID, order, _ := loadLocalModels()
	if len(byID) == 0 {
		for i := range cloud {
			cloud[i].Free = cloud[i].Free || isFreeCredits(cloud[i].Credits)
		}
		return cloud
	}
	out := make([]CachedModel, 0, len(cloud)+len(byID))
	seen := make(map[string]bool, len(cloud))
	for _, m := range cloud {
		if lm, ok := byID[m.ID]; ok {
			m = enrichFromLocal(m, lm)
		}
		m.Free = m.Free || isFreeCredits(m.Credits)
		seen[m.ID] = true
		out = append(out, m)
	}
	for _, id := range order {
		if seen[id] {
			continue
		}
		out = append(out, cachedModelFromLocal(id, byID[id]))
	}
	return out
}

// reasoningFloorTokens is the smallest output budget a thinking model gets.
// It only raises a client's ceiling, never forces generation.
const reasoningFloorTokens = 4096

// reasoningTokenFloor returns the max_tokens a request should be rewritten to,
// or 0 to leave it alone. Thinking models draw their reasoning from the same
// output budget as the answer, so a small budget (16, 64, the 1-token probes
// some clients send) yields reasoning_content and an empty answer.
func reasoningTokenFloor(payload map[string]any) int {
	model, _ := payload["model"].(string)
	if !IsReasoningOnlyModel(model) {
		return 0
	}
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		if v, ok := payload[key].(float64); ok && v > 0 && v < reasoningFloorTokens {
			return reasoningFloorTokens
		}
	}
	return 0
}

// IsReasoningOnlyModel reports whether a model always produces reasoning
// tokens before any content. Those tokens come out of the same max_tokens
// budget, so a small budget yields an empty answer.
func IsReasoningOnlyModel(id string) bool {
	byID, _, _ := loadLocalModels()
	if lm, ok := byID[id]; ok {
		return lm.OnlyReasoning
	}
	globalModelCache.mu.RLock()
	defer globalModelCache.mu.RUnlock()
	for _, m := range globalModelCache.models {
		if m.ID == id {
			return m.OnlyReasoning
		}
	}
	return false
}
