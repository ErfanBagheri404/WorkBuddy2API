package main

import (
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RateLimiter enforces a per-IP token-bucket with random jitter.
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    time.Duration // minimum interval between requests
}

type bucket struct {
	last time.Time
}

// NewRateLimiter creates a limiter allowing one request per interval per IP.
// Jitter of ±20% is applied to the interval to break burst patterns.
func NewRateLimiter(interval time.Duration) *RateLimiter {
	return &RateLimiter{
		buckets: make(map[string]*bucket),
		rate:    interval,
	}
}

// Allow reports whether a request from ip is permitted, and if not, how long
// the caller should wait before retrying.
func (rl *RateLimiter) Allow(ip string) (bool, time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	b, ok := rl.buckets[ip]
	if !ok {
		b = &bucket{}
		rl.buckets[ip] = b
	}

	// base interval ± 20% jitter (signed; effective range [80%, 120%))
	bound := int64(rl.rate) / 5
	if bound < 1 {
		bound = 1
	}
	jitter := time.Duration(rand.Int63n(2*bound) - bound) // [-bound, +bound)
	wait := rl.rate + jitter

	if remaining := wait - now.Sub(b.last); remaining > 0 {
		return false, remaining
	}
	b.last = now
	return true, 0
}

// clientIP extracts the peer IP, stripping the ephemeral port so all requests
// from one host share a bucket. Falls back to X-Forwarded-For when proxied.
func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if i := strings.IndexByte(fwd, ','); i >= 0 {
			return strings.TrimSpace(fwd[:i])
		}
		return strings.TrimSpace(fwd)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// middleware wraps an http.Handler with rate limiting.
func (rl *RateLimiter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, retry := rl.Allow(clientIP(r))
		if !ok {
			secs := (retry + time.Second - 1) / time.Second
			if secs < 1 {
				secs = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(int(secs)))
			http.Error(w, `{"error":{"message":"rate limit exceeded, try again later","type":"rate_limit_error"}}`, http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// startCleanup evicts idle buckets every minute to prevent unbounded growth.
func (rl *RateLimiter) startCleanup() {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			rl.mu.Lock()
			now := time.Now()
			for ip, b := range rl.buckets {
				if now.Sub(b.last) > 5*time.Minute {
					delete(rl.buckets, ip)
				}
			}
			rl.mu.Unlock()
		}
	}()
}
