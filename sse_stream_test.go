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
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	src := &http.Response{
		StatusCode: http.StatusOK,
		Request:    req,
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
		data, ok := sseData(line)
		if !ok {
			t.Fatalf("unexpected non-data SSE line %q in output:\n%s", line, out)
		}
		if data == "[DONE]" {
			doneCount++
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("invalid JSON in SSE data line %q: %v", data, err)
		}
		chunks = append(chunks, chunk)
	}
	return chunks, doneCount
}

// terminalFinishReasons returns choice index → non-empty finish_reason.
func terminalFinishReasons(chunks []map[string]any) map[int]string {
	out := map[int]string{}
	for _, chunk := range chunks {
		choices, _ := chunk["choices"].([]any)
		for _, raw := range choices {
			choice, _ := raw.(map[string]any)
			fr, ok := choice["finish_reason"].(string)
			if !ok || fr == "" {
				continue
			}
			idx := 0
			if f, ok := choice["index"].(float64); ok {
				idx = int(f)
			}
			out[idx] = fr
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
		t.Fatalf("want one terminal finish_reason=length for choice 0, got %v\noutput:\n%s", got, out)
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

// SSE allows `data:{...}` without the space. A terminal chunk in that form must
// be recognized, otherwise a synthetic finish_reason=length is emitted first
// and the completion appears to both stop and truncate.
func TestCopySSELogged_RecognizesTerminalChunkWithoutSpace(t *testing.T) {
	upstream := sseIntermediate +
		"data:{\"id\":\"c1\",\"model\":\"hy4-free\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		sseDone
	out := runCopySSELogged(t, upstream)

	chunks, doneCount := parseSSELines(t, out)
	if doneCount != 1 {
		t.Fatalf("want exactly one [DONE], got %d\noutput:\n%s", doneCount, out)
	}
	if got := terminalFinishReasons(chunks); len(got) != 1 || got[0] != "stop" {
		t.Fatalf("want only the upstream stop reason, got %v\noutput:\n%s", got, out)
	}
}

// Requests with n>1 must terminate every requested choice, not just index 0.
func TestCopySSELogged_TerminatesEveryChoice(t *testing.T) {
	// Choice 1 finishes upstream; choice 0 is left dangling.
	upstream := `data: {"id":"c1","model":"hy4-free","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":""},{"index":1,"delta":{},"finish_reason":"stop"}]}` + "\n\n" + sseDone
	out := runCopySSELogged(t, upstream)

	chunks, doneCount := parseSSELines(t, out)
	if doneCount != 1 {
		t.Fatalf("want exactly one [DONE], got %d\noutput:\n%s", doneCount, out)
	}
	got := terminalFinishReasons(chunks)
	if got[1] != "stop" {
		t.Fatalf("upstream finish_reason for choice 1 must be preserved, got %v", got)
	}
	if got[0] != "length" {
		t.Fatalf("choice 0 must get a synthesized finish_reason=length, got %v\noutput:\n%s", got, out)
	}
}

// A synthesized terminal chunk must belong to the same completion as the
// chunks before it, or clients see two different responses interleaved.
func TestCopySSELogged_SynthesizedChunkKeepsUpstreamIdentity(t *testing.T) {
	upstream := `data: {"id":"upstream-id-42","model":"hy4-free","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":""}]}` + "\n\n"
	out := runCopySSELogged(t, upstream)

	chunks, _ := parseSSELines(t, out)
	if len(chunks) < 2 {
		t.Fatalf("want an upstream chunk plus a synthesized one, got %d\noutput:\n%s", len(chunks), out)
	}
	last := chunks[len(chunks)-1]
	if last["id"] != "upstream-id-42" {
		t.Fatalf("synthesized chunk id = %v, want upstream-id-42", last["id"])
	}
	if last["model"] != "hy4-free" {
		t.Fatalf("synthesized chunk model = %v, want hy4-free", last["model"])
	}
}

// Text that merely looks like a finish_reason inside content or tool arguments
// must survive normalization untouched.
func TestCopySSELogged_NormalizationDoesNotCorruptContent(t *testing.T) {
	tricky := `he said \"finish_reason\":\"\" in the payload`
	upstream := `data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"` + tricky + `"},"finish_reason":""}]}` + "\n\n" + sseTerminal + sseDone
	out := runCopySSELogged(t, upstream)

	chunks, _ := parseSSELines(t, out)
	choices, _ := chunks[0]["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)
	if want := `he said "finish_reason":"" in the payload`; delta["content"] != want {
		t.Fatalf("content was corrupted: got %q want %q", delta["content"], want)
	}
	if choice["finish_reason"] != nil {
		t.Fatalf("finish_reason not normalized: %#v", choice["finish_reason"])
	}
}

// A whitespace layout other than the upstream's usual one must still normalize.
func TestCopySSELogged_NormalizationHandlesArbitraryJSONWhitespace(t *testing.T) {
	upstream := `data:{"choices":[{"index":0,"delta":{},"finish_reason" : ""}]}` + "\n" + sseTerminal + sseDone
	out := runCopySSELogged(t, upstream)

	if strings.Contains(out, `\""`) {
		t.Fatalf("unexpected escaping in output:\n%s", out)
	}
	chunks, _ := parseSSELines(t, out)
	choices, _ := chunks[0]["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	if choice["finish_reason"] != nil {
		t.Fatalf("spaced finish_reason not normalized: %#v", choice["finish_reason"])
	}
}

// An upstream that ends without any data line still needs a terminal so the
// client does not loop on "stream ended without finish_reason".
func TestCopySSELogged_EmptyUpstreamStillTerminates(t *testing.T) {
	out := runCopySSELogged(t, "")

	chunks, doneCount := parseSSELines(t, out)
	if doneCount != 1 {
		t.Fatalf("want exactly one [DONE], got %d\noutput:\n%s", doneCount, out)
	}
	if got := terminalFinishReasons(chunks); got[0] != "length" {
		t.Fatalf("want synthesized finish_reason=length for choice 0, got %v", got)
	}
}
