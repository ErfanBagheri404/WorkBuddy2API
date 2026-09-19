package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientIP_StripsPort(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.168.1.5:54321"
	if got := clientIP(req); got != "192.168.1.5" {
		t.Fatalf("got %q want 192.168.1.5", got)
	}
}

func TestClientIP_PrefersXForwardedFor(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	if got := clientIP(req); got != "203.0.113.9" {
		t.Fatalf("got %q want 203.0.113.9", got)
	}
}

func TestRateLimiter_BlocksThenAllows(t *testing.T) {
	rl := NewRateLimiter(50 * time.Millisecond)

	ok, _ := rl.Allow("1.2.3.4")
	if !ok {
		t.Fatal("first request should be allowed")
	}
	ok, retry := rl.Allow("1.2.3.4")
	if ok {
		t.Fatal("immediate second request should be blocked")
	}
	if retry <= 0 {
		t.Fatalf("retry should be positive, got %v", retry)
	}
	if ok, _ := rl.Allow("5.6.7.8"); !ok {
		t.Fatal("different IP should be allowed")
	}
	time.Sleep(80 * time.Millisecond)
	if ok, _ := rl.Allow("1.2.3.4"); !ok {
		t.Fatal("should be allowed after interval elapsed")
	}
}

func TestRateLimiter_TinyIntervalDoesNotPanic(t *testing.T) {
	rl := NewRateLimiter(1 * time.Nanosecond)
	rl.Allow("1.2.3.4")
	rl.Allow("1.2.3.4")
}

func TestRateLimiter_JitterIsSigned(t *testing.T) {
	rl := NewRateLimiter(1 * time.Hour)
	rl.Allow("x")
	below, above := 0, 0
	for i := 0; i < 400; i++ {
		rl.mu.Lock()
		rl.buckets["x"] = &bucket{last: time.Now()}
		rl.mu.Unlock()
		_, retry := rl.Allow("x")
		if retry < time.Hour {
			below++
		} else {
			above++
		}
	}
	if below == 0 || above == 0 {
		t.Fatalf("jitter not signed: below=%d above=%d", below, above)
	}
}

func TestRateLimiterMiddleware_429AndRetryAfter(t *testing.T) {
	rl := NewRateLimiter(2 * time.Second)
	h := rl.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	call := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "1.2.3.4:1111"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	if w := call(); w.Code != 200 {
		t.Fatalf("first call: got %d", w.Code)
	}
	w := call()
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second call: expected 429, got %d", w.Code)
	}
	ra := w.Header().Get("Retry-After")
	if ra != "2" && ra != "3" {
		t.Fatalf("Retry-After should be >=2 for a 2s interval, got %q", ra)
	}
}
