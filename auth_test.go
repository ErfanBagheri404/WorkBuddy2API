package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func stubHandler(reached *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusOK)
	})
}

func callAuth(t *testing.T, apiKey, path string, headers map[string]string) (int, bool) {
	t.Helper()
	reached := false
	srv := &Server{apiKey: apiKey}
	h := srv.requireAPIKey(stubHandler(&reached))
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code, reached
}

func TestAuth_NoKeyConfigured_AlwaysPasses(t *testing.T) {
	for _, path := range []string{"/v1/models", "/v1/anything", "/healthz"} {
		code, reached := callAuth(t, "", path, nil)
		if !reached || code != 200 {
			t.Fatalf("%s: expected pass-through, got code=%d reached=%v", path, code, reached)
		}
	}
}

func TestAuth_HealthzBypassesAuth(t *testing.T) {
	for _, path := range []string{"/healthz", "/"} {
		code, reached := callAuth(t, "secret", path, nil)
		if !reached || code != 200 {
			t.Fatalf("%s: expected pass-through, got code=%d reached=%v", path, code, reached)
		}
	}
}

func TestAuth_V1RequiresCredential(t *testing.T) {
	for _, path := range []string{"/v1/models", "/v1/chat/completions", "/v1/unknown"} {
		code, reached := callAuth(t, "secret", path, nil)
		if code != http.StatusUnauthorized || reached {
			t.Fatalf("%s: expected 401 and no pass-through, got code=%d reached=%v", path, code, reached)
		}
	}
}

func TestAuth_ValidCredentialsPass(t *testing.T) {
	cases := []map[string]string{
		{"Authorization": "Bearer secret"},
		{"Authorization": "bearer secret"},
		{"Authorization": "BEARER secret"},
		{"Authorization": "Bearer  secret"},
		{"x-api-key": "secret"},
	}
	for _, h := range cases {
		code, reached := callAuth(t, "secret", "/v1/models", h)
		if !reached || code != 200 {
			t.Fatalf("headers %v: expected pass, got code=%d reached=%v", h, code, reached)
		}
	}
}

func TestAuth_InvalidCredentialsRejected(t *testing.T) {
	cases := []map[string]string{
		{"Authorization": "Bearer wrong"},
		{"Authorization": "Basic secret"},
		{"x-api-key": "wrong"},
		{"Authorization": "Bearer wrong", "x-api-key": "secret"},
	}
	for _, h := range cases {
		code, reached := callAuth(t, "secret", "/v1/models", h)
		if code != http.StatusUnauthorized || reached {
			t.Fatalf("headers %v: expected 401, got code=%d reached=%v", h, code, reached)
		}
	}
}

func TestAuth_xApiKeyFallbackWhenBearerMalformed(t *testing.T) {
	h := map[string]string{"Authorization": "Basic junk", "x-api-key": "secret"}
	code, reached := callAuth(t, "secret", "/v1/models", h)
	if !reached || code != 200 {
		t.Fatalf("expected x-api-key fallback to pass, got code=%d reached=%v", code, reached)
	}
}
