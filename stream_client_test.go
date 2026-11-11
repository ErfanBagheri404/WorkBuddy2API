package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// withPlainDial returns the client with a transport that dials addresses
// directly. The production dialer deliberately never dials loopback: it exists
// to reach the WorkBuddy gateway, and falls back to hardcoded upstream IPs when
// DNS misbehaves. Tests need an httptest server on 127.0.0.1, so they swap only
// the dialer and keep the timeouts under test.
func withPlainDial(t *testing.T, c *http.Client) *http.Client {
	t.Helper()
	transport, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client transport = %T, want *http.Transport", c.Transport)
	}
	clone := transport.Clone()
	clone.DialContext = (&net.Dialer{Timeout: 10 * time.Second}).DialContext
	out := *c
	out.Transport = clone
	return &out
}

func TestNewStreamingHTTPClient_HasNoOverallTimeout(t *testing.T) {
	c := newStreamingHTTPClient()
	if c.Timeout != 0 {
		t.Fatalf("streaming client must not set http.Client.Timeout (got %v): it bounds the whole exchange, including reading the SSE body", c.Timeout)
	}
	transport, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("streaming client transport = %T, want *http.Transport", c.Transport)
	}
	if transport.ResponseHeaderTimeout <= 0 {
		t.Fatalf("streaming client needs ResponseHeaderTimeout so a hung upstream cannot pin the request forever")
	}
}

// The bug: http.Client.Timeout cuts the body off mid-stream. The streaming
// client must be able to read a response that lives longer than the old
// fixed deadline.
func TestStreamingHTTPClient_OutlivesFixedDeadline(t *testing.T) {
	const step = 120 * time.Millisecond
	const steps = 4
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < steps; i++ {
			fmt.Fprintf(w, "data: chunk-%d\n\n", i)
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(step)
		}
	}))
	defer srv.Close()

	// Sanity check that the test setup really reproduces the truncation.
	legacy, err := withPlainDial(t, newHTTPClient(step/2)).Get(srv.URL)
	if err == nil {
		_, readErr := io.ReadAll(legacy.Body)
		legacy.Body.Close()
		if readErr == nil {
			t.Fatalf("legacy client with a %v timeout unexpectedly read the whole stream", step/2)
		}
	}

	resp, err := withPlainDial(t, newStreamingHTTPClient()).Get(srv.URL)
	if err != nil {
		t.Fatalf("streaming client request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("streaming client body read: %v", err)
	}
	if want := fmt.Sprintf("chunk-%d", steps-1); !strings.Contains(string(body), want) {
		t.Fatalf("streaming client missed %q, body=%q", want, body)
	}
}

func TestBodyWantsStream(t *testing.T) {
	tests := []struct {
		body string
		want bool
	}{
		{`{"model":"hy4free","stream":true}`, true},
		{`{"model":"hy4free","stream":false}`, false},
		{`{"model":"hy4free"}`, false},
		{``, false},
		{`not json`, false},
	}
	for _, tc := range tests {
		if got := bodyWantsStream([]byte(tc.body)); got != tc.want {
			t.Errorf("bodyWantsStream(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

func TestClientFor_StreamingChatUsesStreamingClient(t *testing.T) {
	am := &AuthManager{
		client:       newHTTPClient(30 * time.Second),
		streamClient: newStreamingHTTPClient(),
	}
	tests := []struct {
		name string
		path string
		body string
		want *http.Client
	}{
		{"streaming chat", "/v2/chat/completions", `{"stream":true}`, am.streamClient},
		{"non-streaming chat", "/v2/chat/completions", `{"stream":false}`, am.client},
		{"models", "/v2/enterprises/personal/models", ``, am.client},
	}
	for _, tc := range tests {
		if got := am.clientFor(tc.path, []byte(tc.body)); got != tc.want {
			t.Errorf("%s: clientFor(%q) picked the wrong client", tc.name, tc.path)
		}
	}
}

// Callers that build an AuthManager without LoadAuthManager must keep working.
func TestClientFor_NilStreamingClientFallsBack(t *testing.T) {
	am := &AuthManager{client: newHTTPClient(30 * time.Second)}
	if got := am.clientFor("/v2/chat/completions", []byte(`{"stream":true}`)); got != am.client {
		t.Fatal("nil streamClient must fall back to the regular client")
	}
}

// idleReadCloser must cancel the request when the upstream goes quiet, so a
// body that stalls after the headers cannot pin the handler forever. Cancelling
// a derived context would not unblock the read; the request's own context must
// be the one cancelled.
func TestIdleReadCloser_CancelsStalledBody(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	body := &stallingBody{ctx: ctx, first: []byte("data: first\n")}
	timer := time.AfterFunc(50*time.Millisecond, cancel)
	r := &idleReadCloser{body: body, cancel: cancel, idle: 50 * time.Millisecond, timer: timer}
	defer r.Close()

	first := make([]byte, 64)
	if _, err := r.Read(first); err != nil {
		t.Fatalf("first read: %v", err)
	}
	// The timer is what unblocks the read -- not the test's own cleanup.
	timer.Reset(50 * time.Millisecond)

	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 64)
		_, err := r.Read(buf)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stalled read returned no error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stalled read never unblocked: idle deadline did not fire")
	}
}

// stallingBody returns one chunk, then blocks until the request context is
// cancelled -- the behaviour of reading a stalled HTTP response body.
type stallingBody struct {
	ctx   context.Context
	first []byte
	sent  bool
}

func (b *stallingBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, b.first), nil
	}
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *stallingBody) Close() error { return nil }
