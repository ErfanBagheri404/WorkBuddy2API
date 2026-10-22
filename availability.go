package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// WorkBuddy gates models per account type: a free account gets
// {"code":11158,"msg":"model [x] is not allowed for user type [...]"} for every
// premium model, and retired models answer 11102 "service info not found".
// Neither is transient, so advertising those ids in /v1/models only produces
// broken picks downstream. The proxy records a verdict per (account, model):
// passively from real traffic (costs nothing) and from a background probe of
// the models nobody has used yet. Verdicts expire so an account upgrade or a
// model re-release puts the id back on the list on its own.

const (
	modelStatusOK     = "ok"
	modelStatusDenied = "denied" // 11158 not allowed for this user type
	modelStatusGone   = "gone"   // 11102 service info not found

	// availabilityTTL keeps a negative verdict from outliving a plan upgrade.
	availabilityTTL = 7 * 24 * time.Hour

	probeEnv = "WORKBUDDY2API_MODEL_PROBE"
)

type modelVerdict struct {
	Status string `json:"status"`
	At     int64  `json:"at"`
}

type availabilityStore struct {
	mu     sync.Mutex
	path   string
	loaded bool
	data   map[string]map[string]modelVerdict
}

var modelAvailability = &availabilityStore{}

func availabilityPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "."+availabilityFile)
}

const availabilityFile = "workbuddy2api-model-availability.json"

func (s *availabilityStore) loadLocked() {
	if s.loaded {
		return
	}
	s.loaded = true
	s.data = map[string]map[string]modelVerdict{}
	if s.path == "" {
		s.path = availabilityPath()
	}
	if s.path == "" {
		return
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	parsed := map[string]map[string]modelVerdict{}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return
	}
	for uid, models := range parsed {
		if len(models) > 0 {
			s.data[uid] = models
		}
	}
}

func (s *availabilityStore) saveLocked() {
	if s.path == "" {
		return
	}
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, s.path)
}

// verdictLocked returns the stored verdict for (uid, model), dropping entries
// past the TTL.
func (s *availabilityStore) verdictLocked(uid, model string) (modelVerdict, bool) {
	models, ok := s.data[uid]
	if !ok {
		return modelVerdict{}, false
	}
	v, ok := models[model]
	if !ok {
		return modelVerdict{}, false
	}
	if time.Since(time.Unix(v.At, 0)) > availabilityTTL {
		delete(models, model)
		if len(models) == 0 {
			delete(s.data, uid)
		}
		return modelVerdict{}, false
	}
	return v, true
}

// RecordModelVerdict remembers what upstream said about a model for an account.
func RecordModelVerdict(uid, model, status string) {
	if uid == "" || model == "" || status == "" {
		return
	}
	s := modelAvailability
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	if s.data[uid] == nil {
		s.data[uid] = map[string]modelVerdict{}
	}
	if prev, ok := s.data[uid][model]; ok && prev.Status == status {
		return
	}
	s.data[uid][model] = modelVerdict{Status: status, At: time.Now().Unix()}
	s.saveLocked()
}

// ModelUsable reports whether the account is known to be unable to use a
// model. Unknown models count as usable until something proves otherwise.
func ModelUsable(uid, model string) bool {
	if uid == "" {
		return true
	}
	s := modelAvailability
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	v, ok := s.verdictLocked(uid, model)
	if !ok {
		return true
	}
	return v.Status == modelStatusOK
}

// FilterUsableModels drops the models this account cannot use.
func FilterUsableModels(uid string, models []CachedModel) []CachedModel {
	if uid == "" || len(models) == 0 {
		return models
	}
	s := modelAvailability
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	out := make([]CachedModel, 0, len(models))
	for _, m := range models {
		if v, ok := s.verdictLocked(uid, m.ID); ok && v.Status != modelStatusOK {
			continue
		}
		out = append(out, m)
	}
	return out
}

// ClassifyModelError maps an upstream chat failure to a verdict. Only the
// stable, account-level errors are classified; everything else (quota, rate
// limits, bad parameters, timeouts) stays "unknown" so it never hides a model.
func ClassifyModelError(statusCode int, body []byte) string {
	if statusCode < 400 || len(body) == 0 {
		return ""
	}
	text := string(body)
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(text, "11158"), strings.Contains(lower, "is not allowed for user type"):
		return modelStatusDenied
	case strings.Contains(text, "11102"), strings.Contains(lower, "service info not found"):
		return modelStatusGone
	}
	return ""
}

var probeRunning sync.Map // uid -> struct{}

// probeEnabled reports whether background probing is on. Default on; set
// WORKBUDDY2API_MODEL_PROBE=0 to keep the advertised list untouched.
func probeEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(probeEnv))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// MaybeProbeModels learns the verdict for models that have never been used on
// this account, one request per model, in the background. A model that answers
// 200 is usable even when the answer is empty (reasoning models spend a
// 1-token budget on thinking), so only the error codes are classified.
func MaybeProbeModels(client *UpstreamClient, uid string, models []CachedModel) {
	if !probeEnabled() || uid == "" || client == nil || len(models) == 0 {
		return
	}
	if _, busy := probeRunning.LoadOrStore(uid, struct{}{}); busy {
		return
	}
	go func() {
		defer probeRunning.Delete(uid)
		for _, m := range models {
			if !needsProbe(uid, m.ID) {
				continue
			}
			probeModel(client, uid, m.ID)
		}
	}()
}

func needsProbe(uid, model string) bool {
	s := modelAvailability
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	_, known := s.verdictLocked(uid, model)
	return !known
}

func probeModel(client *UpstreamClient, uid, model string) {
	body, err := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []any{map[string]any{"role": "user", "content": "ping"}},
		"max_tokens": 1,
	})
	if err != nil {
		return
	}
	resp, err := client.chatWithOptions(body, chatOptions{skipReasoningFloor: true})
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		RecordModelVerdict(uid, model, modelStatusOK)
		return
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if verdict := ClassifyModelError(resp.StatusCode, raw); verdict != "" {
		RecordModelVerdict(uid, model, verdict)
	}
}

// availabilitySummary is used by the status screen.
func availabilitySummary(uid string) string {
	s := modelAvailability
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	models := s.data[uid]
	if len(models) == 0 {
		return ""
	}
	var denied, gone int
	for _, v := range models {
		switch v.Status {
		case modelStatusDenied:
			denied++
		case modelStatusGone:
			gone++
		}
	}
	return fmt.Sprintf("%d hidden (not on this plan: %d, retired: %d)", denied+gone, denied, gone)
}

var _ = http.StatusOK
