package main

import (
	"fmt"
	"strings"
)

// Version is injected at build time:
//   go build -ldflags "-X main.Version=v0.1.0"
var Version = "dev"

func printBanner() {
	fmt.Println()
	fmt.Println(" ╔══════════════════════════════════════╗")
	fmt.Printf(" ║%s║\n", centerText("WorkBuddy2API "+Version, 38))
	fmt.Printf(" ║%s║\n", centerText("OpenAI-compatible proxy for", 38))
	fmt.Printf(" ║%s║\n", centerText("WorkBuddy AI (www.workbuddy.ai)", 38))
	fmt.Println(" ╚══════════════════════════════════════╝")
}

func centerText(s string, width int) string {
	if len(s) >= width {
		return s[:width]
	}
	left := (width - len(s)) / 2
	right := width - len(s) - left
	return strings.Repeat(" ", left) + s + strings.Repeat(" ", right)
}
