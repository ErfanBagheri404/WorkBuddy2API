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
	if headless {
		runHeadless(apiKey)
		return
	}
	runInteractive(apiKey)
}

// flagValue returns the value of --name=value or --name value, falling back
// to the given environment variable.
func flagValue(name, env string) string {
	args := os.Args[1:]
	for i, arg := range args {
		if strings.HasPrefix(arg, name+"=") {
			return strings.TrimPrefix(arg, name+"=")
		}
		if arg == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return os.Getenv(env)
}

func runInteractive(apiKey string) {
	printBanner()

	authFile := defaultAuthPath()

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
		}
		fmt.Println()
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

func startServer(authFile, apiKey string) {
	fmt.Println()
	am, err := LoadAuthManager(authFile)
	if err != nil {
		fmt.Printf("  ✗ Auth error: %v\n\n", err)
		return
	}
	client := NewUpstreamClient(am)
	srv := NewServer(client, am, apiKey)

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
	srv := NewServer(client, am, apiKey)
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
