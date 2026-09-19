package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	var headless bool
	for _, arg := range os.Args[1:] {
		switch {
		case arg == "--headless":
			headless = true
		case arg == "--api-key":
			// handled by flagValue below
		case strings.HasPrefix(arg, "--api-key="):
			// handled by flagValue below
		}
	}
	apiKey := flagValue("--api-key", "WORKBUDDY2API_KEY")
	optDesensitize = boolFlag("--desensitize", "WORKBUDDY2API_DESENSITIZE")
	optRateLimit = parseInterval(flagValue("--rate-limit", "WORKBUDDY2API_RATE_LIMIT"))
	forceImport = boolFlag("--import-creds", "WORKBUDDY2API_IMPORT_CREDS")
	optAccount = flagValue("--account", "WORKBUDDY2API_ACCOUNT")
	if err := EnableLogging(flagValue("--log", "WORKBUDDY2API_LOG")); err != nil {
		fmt.Fprintf(os.Stderr, "logging disabled: %v\n", err)
	}
	// --import-creds is an init-only operation: import, report, exit. Keeping it
	// here (and returning) means the import never runs twice and headless mode
	// never starts a server on the back of an import-only invocation.
	if forceImport {
		authFile := defaultAuthPath()
		src, err := ImportDesktopCredentials(authFile, true)
		if err != nil {
			fmt.Fprintf(os.Stderr, "import credentials: %v\n", err)
			os.Exit(1)
		}
		if src == "" {
			fmt.Fprintln(os.Stderr, "no WorkBuddy desktop credentials found")
			os.Exit(1)
		}
		fmt.Printf("imported credentials from %s\n", src)
		return
	}
	if headless {
		runHeadless(apiKey)
		return
	}
	runInteractive(apiKey)
}

// optDesensitize / optRateLimit / forceImport / optAccount are process-wide flags.
var (
	optDesensitize bool
	optRateLimit   time.Duration
	forceImport    bool
	optAccount     string
)

// parseInterval accepts "2s"/"500ms" or a bare number of seconds.
// Empty or unparseable input disables the limiter.
func parseInterval(s string) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if n, err := strconv.Atoi(s); err == nil {
		if n <= 0 {
			return 0
		}
		return time.Duration(n) * time.Second
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// newLimiter builds the rate limiter, or nil when disabled.
func newLimiter() *RateLimiter {
	if optRateLimit <= 0 {
		return nil
	}
	rl := NewRateLimiter(optRateLimit)
	rl.startCleanup()
	return rl
}

// boolFlag reports whether a valueless switch was passed, or whether the
// environment variable holds a truthy value. Accepts --name, --name=true, and
// --name=false so a scripted invocation can explicitly disable it.
func boolFlag(name, env string) bool {
	for _, arg := range os.Args[1:] {
		if arg == name {
			return true
		}
		if v, ok := strings.CutPrefix(arg, name+"="); ok {
			switch strings.ToLower(v) {
			case "0", "false", "no":
				return false
			default:
				return true
			}
		}
	}
	switch strings.ToLower(os.Getenv(env)) {
	case "", "0", "false", "no":
		return false
	}
	return true
}

// flagValue returns the value of --name=value or --name value, falling back
// to the given environment variable.
func flagValue(name, env string) string {
	args := os.Args[1:]
	for i, arg := range args {
		if strings.HasPrefix(arg, name+"=") {
			return strings.TrimPrefix(arg, name+"=")
		}
		if arg == name && i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
			return args[i+1]
		}
	}
	return os.Getenv(env)
}

func runInteractive(apiKey string) {
	printBanner()

	authFile := defaultAuthPath()

	// --import-creds returns in main(); reaching runInteractive means it is not
	// set.  When no stored credentials exist, try the desktop app silently.
	if !hasStoredAuth(authFile) {
		if src, err := ImportDesktopCredentials(authFile, false); err == nil && src != "" {
			fmt.Printf("  Imported existing WorkBuddy desktop credentials from\n    %s\n\n", src)
		}
	}

	if !hasStoredAuth(authFile) {
		fmt.Println("  No credentials found.")
		fmt.Println()
		fmt.Println("  Press Enter to open the browser and log in...")
		waitEnter()
		if err := runLogin(authFile); err != nil {
			fmt.Fprintf(os.Stderr, "\n  Login failed: %v\n\n", err)
			waitEnter()
			return
		}
		fmt.Println("  Logged in successfully!")
		fmt.Println()
	}

	for {
		printMenu()
		choice := readChoice("  > ")

		switch choice {
		case 1:
			showStatus(authFile)
		case 2:
			startServer(authFile, apiKey)
		case 3:
			showModels(authFile)
		case 4:
			testChat(authFile)
		case 5:
			fmt.Println()
			fmt.Println("  Opening browser for re-login...")
			time.Sleep(500 * time.Millisecond)
			if err := runLogin(authFile); err != nil {
				fmt.Fprintf(os.Stderr, "\n  Login failed: %v\n\n", err)
			} else {
				fmt.Println("  Logged in successfully!")
			}
			fmt.Println()
		case 6:
			fmt.Println("  Bye!")
			return
		default:
			fmt.Println("  Invalid choice. Try again.")
			fmt.Println()
		}
	}
}

func printMenu() {
	fmt.Println("  ┌─────────────────────────────────┐")
	fmt.Println("  │  1. Status                      │")
	fmt.Println("  │  2. Start server                │")
	fmt.Println("  │  3. List models                 │")
	fmt.Println("  │  4. Test chat                   │")
	fmt.Println("  │  5. Re-login                    │")
	fmt.Println("  │  6. Quit                        │")
	fmt.Println("  └─────────────────────────────────┘")
	fmt.Println()
}

func readChoice(prompt string) int {
	fmt.Print(prompt)
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(line)
	n, _ := strconv.Atoi(line)
	return n
}

func waitEnter() {
	fmt.Print("  Press Enter...")
	bufio.NewReader(os.Stdin).ReadString('\n')
}

func showStatus(authFile string) {
	fmt.Println()
	am, err := LoadAuthManager(authFile)
	if err != nil {
		fmt.Printf("  ✗ Auth error: %v\n\n", err)
		return
	}
	fmt.Println("  ✓ Authenticated")
	fmt.Printf("    User ID : %s\n", am.uid)
	fmt.Printf("    Domain  : %s\n", am.domain)
	fmt.Printf("    Token   : %d chars, expires %s\n",
		len(am.data.Auth.AccessToken),
		time.UnixMilli(am.data.Auth.ExpiresAt).Format("2006-01-02 15:04:05"),
	)
	fmt.Println()
	fmt.Println("  Models:")
	models, err := GetModels(am)
	if err != nil {
		fmt.Printf("    (could not fetch: %v)\n", err)
	} else {
		for _, m := range models {
			def := ""
			if m.Default {
				def = " (default)"
			}
			fmt.Printf("    - %s  %s%s\n", m.ID, m.Name, def)
		}
	}
	fmt.Println()
}

func showModels(authFile string) {
	fmt.Println()
	am, err := LoadAuthManager(authFile)
	if err != nil {
		fmt.Printf("  ✗ Auth error: %v\n\n", err)
		return
	}
	fmt.Println("  Fetching models from WorkBuddy...")
	fmt.Println()
	models, err := GetModels(am)
	if err != nil {
		fmt.Printf("  ✗ Fetch failed: %v\n\n", err)
		return
	}
	for _, m := range models {
		def := ""
		if m.Default {
			def = " (default)"
		}
		fmt.Printf("    - %s  %s%s\n", m.ID, m.Name, def)
		if caps := modelCapabilities(m); caps != "" {
			fmt.Printf("        %s\n", caps)
		}
	}
	fmt.Println()
}

// modelCapabilities renders the capability and context-window badges for the
// CLI model list.
func modelCapabilities(m CachedModel) string {
	var parts []string
	if m.MaxInputTokens > 0 {
		parts = append(parts, fmt.Sprintf("in=%d", m.MaxInputTokens))
	}
	if m.MaxOutputTokens > 0 {
		parts = append(parts, fmt.Sprintf("out=%d", m.MaxOutputTokens))
	}
	if m.SupportsImages {
		parts = append(parts, "images")
	}
	if m.SupportsToolCall {
		parts = append(parts, "tools")
	}
	if m.SupportsReasoning {
		parts = append(parts, "reasoning")
	}
	return strings.Join(parts, " ")
}

func testChat(authFile string) {
	fmt.Println()
	am, err := LoadAuthManager(authFile)
	if err != nil {
		fmt.Printf("  ✗ Auth error: %v\n\n", err)
		return
	}
	models, err := GetModels(am)
	defaultModel := "default-model"
	if err == nil && len(models) > 0 {
		defaultModel = DefaultModel(models)
	}
	fmt.Print("  Enter model [" + defaultModel + "]: ")
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		line = defaultModel
	}
	model := line

	fmt.Print("  Enter message: ")
	line, _ = reader.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		line = "Hello, reply with a short greeting."
	}

	fmt.Printf("\n  Chatting with %s...\n\n", model)

	payload := fmt.Sprintf(`{"model":"%s","stream":true,"messages":[{"role":"system","content":"You are a helpful assistant."},{"role":"user","content":%s}],"max_tokens":512}`,
		model, jsonEscapeString(line))
	resp, err := am.doUpstream(http.MethodPost, "/v2/chat/completions", []byte(payload), map[string]string{"Accept": "text/event-stream"})
	if err != nil {
		fmt.Printf("  ✗ Request failed: %v\n\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		fmt.Printf("  ✗ HTTP %d: %s\n\n", resp.StatusCode, truncate(string(raw), 200))
		return
	}

	decoder := json.NewDecoder(resp.Body)
	for {
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := decoder.Decode(&chunk); err != nil {
			break
		}
		if len(chunk.Choices) > 0 {
			if chunk.Choices[0].Delta.Content != "" {
				fmt.Print(chunk.Choices[0].Delta.Content)
			}
			if chunk.Choices[0].FinishReason != nil {
				break
			}
		}
	}
	fmt.Println()
	fmt.Println()
}

// newClientForAuth builds the upstream client for a server run. When multiple
// account files exist under ~/.workbuddy2api/accounts/, it returns a
// pool-backed client with round-robin selection and quota failover; otherwise
// it falls back to the single-account client.
func newClientForAuth(authFile string) (*UpstreamClient, *AuthManager, error) {
	pool, perr := LoadAccountPool(optAccount)
	if perr == nil && pool.Count() > 1 {
		fmt.Printf("  Accounts: %d (%s)\n", pool.Count(), strings.Join(pool.All(), ", "))
		if optAccount != "" {
			fmt.Printf("  Pinned to: %s\n", optAccount)
		}
		am := pool.Peek()
		return NewUpstreamClientPool(pool), am, nil
	}
	am, err := LoadAuthManager(authFile)
	if err != nil {
		return nil, nil, err
	}
	return NewUpstreamClient(am), am, nil
}

func startServer(authFile, apiKey string) {
	fmt.Println()
	client, am, err := newClientForAuth(authFile)
	if err != nil {
		fmt.Printf("  ✗ Auth error: %v\n\n", err)
		return
	}
	srv := NewServer(client, am, apiKey, optDesensitize, newLimiter())

	addr := ":61021"
	fmt.Printf("  Starting server on http://localhost%s\n", addr)
	if apiKey != "" {
		fmt.Println("  API key auth: ENABLED (Authorization: Bearer <key>)")
	} else {
		fmt.Println("  API key auth: disabled (localhost only)")
	}
	fmt.Println()
	fmt.Println("  Endpoints:")
	fmt.Printf("    GET  http://localhost%s/healthz\n", addr)
	fmt.Printf("    GET  http://localhost%s/v1/models\n", addr)
	fmt.Printf("    POST http://localhost%s/v1/chat/completions\n", addr)
	fmt.Println()
	fmt.Println("  Press Ctrl+C to stop.")
	fmt.Println()

	if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
		fmt.Fprintf(os.Stderr, "  Server error: %v\n", err)
		waitEnter()
	}
}

func runHeadless(apiKey string) {
	authFile := defaultAuthPath()
	am, err := LoadAuthManager(authFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "auth error: %v\n", err)
		os.Exit(1)
	}
	client := NewUpstreamClient(am)
	srv := NewServer(client, am, apiKey, optDesensitize, newLimiter())
	addr := ":61021"
	fmt.Printf("WorkBuddy2API listening on http://localhost%s\n", addr)
	if apiKey != "" {
		fmt.Println("API key auth enabled")
	}
	if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}
}

func jsonEscapeString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
