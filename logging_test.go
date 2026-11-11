package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedact_CoversHeaderAndCamelCaseKeys(t *testing.T) {
	// Built at runtime so no token-shaped literal exists in the source tree.
	fake := strings.Repeat("z", 24)
	val := strings.Repeat("q", 16)
	tests := []string{
		`{"api_key":"` + val + `"}`,
		`{"apiKey":"` + val + `"}`,
		`{"api-key":"` + val + `"}`,
		`{"x-api-key":"` + val + `"}`,
		`{"accessToken":"` + val + `"}`,
		`{"refresh_token":"` + val + `"}`,
		`{"password":"` + val + `"}`,
		"Authorization: Bearer " + fake,
		"authorization: token " + fake,
	}
	for _, in := range tests {
		got := redact(in)
		if strings.Contains(got, val) || strings.Contains(got, fake) {
			t.Errorf("secret survived redaction: %q -> %q", in, got)
		}
	}
}

func TestRedact_LeavesOrdinaryTextAlone(t *testing.T) {
	in := `{"model":"default-model","messages":[{"role":"user","content":"hello"}]}`
	if got := redact(in); got != in {
		t.Errorf("ordinary payload was altered: %q", got)
	}
}

func TestFinishReasonFromLine_HandlesSplitEvents(t *testing.T) {
	// A line that is only a fragment of an event must not overwrite state.
	fragment := `data: {"choices":[{"finish_rea`
	if got := finishReasonFromLine(fragment, "stop"); got != "stop" {
		t.Errorf("fragment changed finish reason: %q", got)
	}
	// A complete line with the real value must be picked up.
	full := `data: {"choices":[{"finish_reason":"length"}]}`
	if got := finishReasonFromLine(full, "stop"); got != "length" {
		t.Errorf("complete line ignored: %q", got)
	}
	// [DONE] is not a finish reason.
	if got := finishReasonFromLine("data: [DONE]", "length"); got != "length" {
		t.Errorf("[DONE] overwrote finish reason: %q", got)
	}
}

func TestFinishReasonFromLine_AcceptsSpaceAfterColonVariants(t *testing.T) {
	// SSE permits zero or more spaces after "data:". The gateway uses one,
	// but a spec-compliant upstream may use none, so terminal detection must
	// not depend on that detail.
	for _, line := range []string{
		`data:{"choices":[{"index":0,"finish_reason":"stop"}]}`,
		`data:  {"choices":[{"index":0,"finish_reason":"stop"}]}`,
		`data: {"choices":[{"index":0,"finish_reason":"stop"}]}`,
	} {
		if got := finishReasonFromLine(line, ""); got != "stop" {
			t.Errorf("terminal chunk not recognized in %q: got %q", line, got)
		}
	}
}

func TestChoiceFinishReasons_PerChoiceTerminalState(t *testing.T) {
	line := `data: {"choices":[{"index":0,"finish_reason":"stop"},{"index":1,"finish_reason":""},{"index":2,"finish_reason":"length"}]}`
	got := choiceFinishReasons(line)
	if len(got) != 2 || got[0] != "stop" || got[2] != "length" {
		t.Fatalf("want terminal reasons for choices 0 and 2 only, got %v", got)
	}
	if _, ok := got[1]; ok {
		t.Fatalf("empty finish_reason must not count as terminal, got %v", got)
	}
	if r := choiceFinishReasons(`data:{"choices":[{"index":1,"finish_reason":"stop"}]}`); r[1] != "stop" {
		t.Fatalf("no-space data line not recognized: %v", r)
	}
	if r := choiceFinishReasons("data: [DONE]"); r != nil {
		t.Fatalf("[DONE] must yield no per-choice reasons, got %v", r)
	}
	if r := choiceFinishReasons(`data: {"choices":[{"finish_rea`); r != nil {
		t.Fatalf("fragment must yield no per-choice reasons, got %v", r)
	}
}

func TestEnableLogging_EmptyPathDisables(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trace.log")

	if err := EnableLogging(path); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !LoggingEnabled() {
		t.Fatal("logging should be enabled after EnableLogging(path)")
	}
	if err := EnableLogging(""); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if LoggingEnabled() {
		t.Fatal("logging should be disabled after EnableLogging(\"\")")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("trace file should still exist: %v", err)
	}
}

func TestLogBlock_TagsEveryLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trace.log")
	if err := EnableLogging(path); err != nil {
		t.Fatalf("enable: %v", err)
	}
	defer EnableLogging("")

	logBlock("abc12345", "REQUEST BODY", "line one\nline two\nline three")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, ln := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if !strings.Contains(ln, "[abc12345]") {
			t.Errorf("line missing correlation id: %q", ln)
		}
	}
	if !strings.Contains(string(data), "line three") {
		t.Errorf("body content missing from trace: %q", string(data))
	}
}
