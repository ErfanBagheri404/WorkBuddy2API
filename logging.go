package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Logger writes per-request traces to a file. Disabled when path is empty.
type Logger struct {
	mu   sync.Mutex
	f    *os.File
	path string
}

var reqLogger = &Logger{}

// EnableLogging opens (or creates) the trace file. Empty path disables logging,
// closing and clearing any file that was already open.
func EnableLogging(path string) error {
	reqLogger.mu.Lock()
	defer reqLogger.mu.Unlock()

	if path == "" {
		if reqLogger.f != nil {
			reqLogger.f.Close()
			reqLogger.f = nil
			reqLogger.path = ""
		}
		return nil
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	if reqLogger.f != nil {
		reqLogger.f.Close()
	}
	reqLogger.f = f
	reqLogger.path = path
	return nil
}

// LoggingEnabled reports whether a trace file is open.
func LoggingEnabled() bool {
	reqLogger.mu.Lock()
	defer reqLogger.mu.Unlock()
	return reqLogger.f != nil
}

// newCorrelationID returns a short random id used to tie the lines of a single
// request together.
func newCorrelationID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(b)
}

// redact strips anything that looks like a credential from a string before it
// reaches the log file.
var (
	reBearer = regexp.MustCompile(`(?i)((?:bearer|token)\s+)[A-Za-z0-9._\-]{8,}`)
	// Covers JSON and header spellings (api_key, api-key, apiKey, x-api-key,
	// accessToken, ...) with either quote style and either separator.
	reToken = regexp.MustCompile(
		`(?i)(["']?(?:access_?token|refresh_?token|accesstoken|refreshtoken|` +
			`authorization|api[-_]?key|x-api-key|password)["']?\s*[:=]\s*)` +
			`["']?[^"',\s}]{4,}["']?`)
)

func redact(s string) string {
	s = reBearer.ReplaceAllString(s, "${1}[REDACTED]")
	s = reToken.ReplaceAllString(s, "${1}[REDACTED]")
	return s
}

// logLine writes a single timestamped, correlation-tagged line.
func logLine(cid, format string, args ...any) {
	reqLogger.mu.Lock()
	defer reqLogger.mu.Unlock()
	if reqLogger.f == nil {
		return
	}
	msg := redact(fmt.Sprintf(format, args...))
	fmt.Fprintf(reqLogger.f, "[%s] [%s] %s\n",
		time.Now().Format("2006-01-02 15:04:05"), cid, msg)
}

// logBlock writes a labelled body, one timestamped record per line so a
// multi-line payload can never be mistaken for independent log records and
// every line carries its own correlation id.
func logBlock(cid, label, body string) {
	if !LoggingEnabled() {
		return
	}
	logLine(cid, "── %s ──", label)
	for _, ln := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		logLine(cid, "  %s", ln)
	}
}

// summariseRequest pulls the fields worth showing on the request line.
func summariseRequest(body []byte) (model string, stream bool, msgs int, lastUser string) {
	var p struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &p) != nil {
		return "", false, 0, ""
	}
	model = p.Model
	stream = p.Stream
	msgs = len(p.Messages)
	for i := len(p.Messages) - 1; i >= 0; i-- {
		if p.Messages[i].Role != "user" {
			continue
		}
		var s string
		if json.Unmarshal(p.Messages[i].Content, &s) == nil {
			lastUser = s
		} else {
			lastUser = string(p.Messages[i].Content)
		}
		break
	}
	if len(lastUser) > 120 {
		lastUser = lastUser[:120] + "..."
	}
	return
}

// finishReasonFromLine extracts finish_reason from one complete SSE data line.
// Callers must pass whole lines: a chunk boundary can split an event, and
// parsing partial JSON would silently drop the upstream finish reason.
func finishReasonFromLine(line, current string) string {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data: ") {
		return current
	}
	data := strings.TrimPrefix(line, "data: ")
	if data == "[DONE]" {
		return current
	}
	var c struct {
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal([]byte(data), &c) != nil {
		return current
	}
	for _, ch := range c.Choices {
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			current = *ch.FinishReason
		}
	}
	return current
}
