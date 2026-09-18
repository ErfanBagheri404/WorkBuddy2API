package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
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

// EnableLogging opens (or creates) the trace file. Empty path disables logging.
func EnableLogging(path string) error {
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	reqLogger.mu.Lock()
	defer reqLogger.mu.Unlock()
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
	reBearer = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._\-]{8,}`)
	reToken  = regexp.MustCompile(`(?i)"(access_?token|refresh_?token|accessToken|refreshToken|authorization|api_?key|password)"\s*:\s*"[^"]*"`)
)

func redact(s string) string {
	s = reBearer.ReplaceAllString(s, "${1}[REDACTED]")
	s = reToken.ReplaceAllString(s, `"$1":"[REDACTED]"`)
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

// logBlock writes a labelled multi-line body (request or response).
func logBlock(cid, label, body string) {
	if !LoggingEnabled() {
		return
	}
	if len(body) > 200000 {
		body = body[:200000] + "\n...[truncated]"
	}
	logLine(cid, "── %s ──\n%s", label, redact(body))
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
