package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Chunks mirroring what the WorkBuddy gateway actually emits: intermediate
// chunks carry a non-standard empty finish_reason and there is no terminal
// chunk of their own (see the 30s-truncation bug).
const (
	sseIntermediate = `data: {"id":"c1","object":"chat.completion.chunk","model":"hy4-free","choices":[{"index":0,"delta":{"content":"","reasoning_content":"thinking"},"finish_reason":""}]}` + "\n\n"
	sseTerminal     = `data: {"id":"c1","object":"chat.completion.chunk","model":"hy4-free","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	sseDone         = "data: [DONE]\n\n"
)

func runCopySSELogged(t *testing.T, upstream string) string {
	t.Helper()
	src := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(upstream)),
	}
	rec := httptest.NewRecorder()
	copySSELogged(rec, src, "cid-1", "hy4free", time.Now())
	return rec.Body.String()
}

// parseSSELines decodes every data: line the proxy emitted. It fails on lines
// that are not valid SSE data payloads, so a malformed synthesized chunk is
// caught here too.
func parseSSELines(t *testing.T, out string) (chunks []map[string]any, doneCount int) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			t.Fatalf("unexpected non-data SSE line %q in output:\n%s", line, out)
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			doneCount++
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("invalid JSON in SSE data line %q: %v", payload, err)
		}
		chunks = append(chunks, chunk)
	}
	return chunks, doneCount
}

// terminalFinishReasons returns every non-empty finish_reason in the stream.
func terminalFinishReasons(chunks []map[string]any) []string {
	var out []string
	for _, chunk := range chunks {
		choices, _ := chunk["choices"].([]any)
		for _, raw := range choices {
			choice, _ := raw.(map[string]any)
			if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
				out = append(out, fr)
			}
		}
	}
	return out
}

// This is the regression test for the 30s truncation: upstream dies mid-stream
// with only intermediate chunks. The client must still receive a well-formed
// ending, otherwise it reports "stream ended without finish_reason" and retries
// work that already produced output.
func TestCopySSELogged_MidStreamDeathEmitsTerminalChunkAndDONE(t *testing.T) {
	out := runCopySSELogged(t, sseIntermediate+sseIntermediate)

	chunks, doneCount := parseSSELines(t, out)
	if doneCount != 1 {
		t.Fatalf("want exactly one [DONE], got %d\noutput:\n%s", doneCount, out)
	}
	if got := terminalFinishReasons(chunks); len(got) != 1 || got[0] != "length" {
		t.Fatalf("want one terminal finish_reason=length, got %v\noutput:\n%s", got, out)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]") {
		t.Fatalf("stream must end with [DONE], output:\n%s", out)
	}
}

// Upstream can close right after the terminal chunk without sending [DONE].
func TestCopySSELogged_AppendsDONEAfterUpstreamFinishReason(t *testing.T) {
	out := runCopySSELogged(t, sseIntermediate+sseTerminal)

	chunks, doneCount := parseSSELines(t, out)
	if doneCount != 1 {
		t.Fatalf("want exactly one [DONE], got %d\noutput:\n%s", doneCount, out)
	}
	got := terminalFinishReasons(chunks)
	if len(got) != 1 || got[0] != "stop" {
		t.Fatalf("want only the upstream finish_reason=stop (no synthetic one), got %v\noutput:\n%s", got, out)
	}
}

// An empty finish_reason is not valid OpenAI schema. Clients that validate the
// stream reject it, so intermediate chunks must carry null instead.
func TestCopySSELogged_NormalizesEmptyFinishReason(t *testing.T) {
	out := runCopySSELogged(t, sseIntermediate+sseTerminal+sseDone)

	if strings.Contains(out, `"finish_reason":""`) {
		t.Fatalf("empty finish_reason leaked to the client:\n%s", out)
	}
	chunks, _ := parseSSELines(t, out)
	if len(chunks) < 2 {
		t.Fatalf("want the upstream chunks preserved, got %d\noutput:\n%s", len(chunks), out)
	}
	choices, _ := chunks[0]["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("first chunk choices malformed: %v", chunks[0])
	}
	choice, _ := choices[0].(map[string]any)
	if value, ok := choice["finish_reason"]; !ok || value != nil {
		t.Fatalf("intermediate finish_reason must be null, got %#v", choice["finish_reason"])
	}
}

// A well-formed upstream stream must pass through without extra chunks or a
// duplicated [DONE].
func TestCopySSELogged_WellFormedStreamPassesThrough(t *testing.T) {
	out := runCopySSELogged(t, sseIntermediate+sseTerminal+sseDone)

	chunks, doneCount := parseSSELines(t, out)
	if doneCount != 1 {
		t.Fatalf("want exactly one [DONE], got %d\noutput:\n%s", doneCount, out)
	}
	if len(chunks) != 2 {
		t.Fatalf("want the 2 upstream chunks and no synthetic one, got %d\noutput:\n%s", len(chunks), out)
	}
	if got := terminalFinishReasons(chunks); len(got) != 1 || got[0] != "stop" {
		t.Fatalf("want one terminal finish_reason=stop, got %v", got)
	}
}

// Two [DONE] sentinels would break clients that treat the first one as the end
// and keep parsing afterwards.
func TestCopySSELogged_NeverEmitsDuplicateDONE(t *testing.T) {
	out := runCopySSELogged(t, sseIntermediate+sseTerminal+sseDone+sseIntermediate)

	if n := strings.Count(out, "[DONE]"); n != 1 {
		t.Fatalf("want exactly one [DONE], got %d\noutput:\n%s", n, out)
	}
}
