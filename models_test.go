package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestToModelObject_PassesCapabilitiesThrough(t *testing.T) {
	m := CachedModel{
		ID:                "test-model",
		Name:              "Test Model",
		Description:       "A model",
		Vendor:            "acme",
		MaxInputTokens:    200000,
		MaxOutputTokens:   8192,
		SupportsImages:    true,
		SupportsToolCall:  true,
		SupportsReasoning: true,
		Credits:           "1.5",
	}
	got := toModelObject(m, 1234)

	if got.ID != "test-model" || got.Object != "model" || got.Created != 1234 || got.OwnedBy != "workbuddy" {
		t.Fatalf("core OpenAI fields wrong: %+v", got)
	}
	if got.MaxInputTokens != 200000 || got.MaxOutputTokens != 8192 {
		t.Errorf("context window lost: %+v", got)
	}
	if !got.SupportsImages || !got.SupportsToolCall || !got.SupportsReasoning {
		t.Errorf("capability flags lost: %+v", got)
	}
	if got.Name != "Test Model" || got.Vendor != "acme" {
		t.Errorf("metadata lost: %+v", got)
	}
}

func TestModelObject_OmitsAbsentCapabilities(t *testing.T) {
	// Minimal model: capability keys must not appear as false/zero, so strict
	// clients see the same shape they saw before this change.
	raw, err := json.Marshal(toModelObject(CachedModel{ID: "bare"}, 1))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{
		"max_input_tokens", "max_output_tokens",
		"supports_images", "supports_tool_calls", "supports_reasoning",
		"credits", "description", "vendor",
	} {
		if _, present := m[k]; present {
			t.Errorf("key %q should be omitted when unset", k)
		}
	}
	for _, k := range []string{"id", "object", "created", "owned_by"} {
		if _, present := m[k]; !present {
			t.Errorf("required OpenAI key %q missing", k)
		}
	}
}

func TestModelByIDRoute_ExtractsID(t *testing.T) {
	tests := []struct {
		path   string
		wantID string
		byID   bool
	}{
		{"/v1/models", "", false},
		{"/v1/models/", "", false},
		{"/v1/models/deepseek-v3", "deepseek-v3", true},
		{"/v1/models/claude-sonnet-4.5", "claude-sonnet-4.5", true},
	}
	for _, tc := range tests {
		r := httptest.NewRequest(http.MethodGet, tc.path, nil)
		id := trimModelID(r.URL.Path)
		if (id != "") != tc.byID {
			t.Errorf("%s: byID=%v want %v", tc.path, id != "", tc.byID)
		}
		if id != tc.wantID {
			t.Errorf("%s: id=%q want %q", tc.path, id, tc.wantID)
		}
	}
}
