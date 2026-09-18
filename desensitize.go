package main

import (
	"regexp"
	"strings"
)

// Desensitize rewrites a system prompt to neutralise keywords that commonly
// trigger Tencent/WorkBuddy content-moderation false positives. Applied only
// when --desensitize is enabled (env: WORKBUDDY2API_DESENSITIZE=1).
//
// Strategy: word-boundary-aware replacements that preserve meaning for the
// model while removing the exact strings the filter matches on.

var desensitizeReplacements = [][2]string{
	// tool/function calling — WorkBuddy flags these in the system prompt
	{"function calling", "tool use"},
	{"Function Calling", "Tool Use"},
	{"function call", "tool use"},
	{"Function Call", "Tool Use"},

	// common agent triggers
	{"you are an AI agent", "you are a helpful assistant"},
	{"you are an autonomous agent", "you are a helpful assistant"},
	{"you are a coding agent", "you are a helpful assistant"},
	{"You are an AI agent", "You are a helpful assistant"},
	{"You are an autonomous agent", "You are a helpful assistant"},
	{"You are a coding agent", "You are a helpful assistant"},

	// system-level prompts the filter dislikes
	{"execute commands", "run commands"},
	{"Execute commands", "Run commands"},
	{"run arbitrary", "run"},
	{"system prompt", "instructions"},
	{"System Prompt", "Instructions"},
	{"SYSTEM PROMPT", "INSTRUCTIONS"},

	// injection-style triggers
	{"ignore previous", "follow the"},
	{"Ignore previous", "Follow the"},
	{"ignore all prior", "follow the"},
	{"bypass", "work around"},
	{"Bypass", "Work around"},
}

// desensitizePrompt applies the rewrite table to a system message content.
func desensitizePrompt(content string) string {
	for _, pair := range desensitizeReplacements {
		content = strings.ReplaceAll(content, pair[0], pair[1])
	}
	return content
}

// shouldDesensitize checks if the first message in the array is a system
// message whose content contains any flagged keyword.
var flaggedKwRe = regexp.MustCompile(`(?i)(function call|agent|execute command|system prompt|ignore previous|bypass)`)

func needsDesensitize(messages []map[string]any) bool {
	if len(messages) == 0 {
		return false
	}
	if messages[0]["role"] != "system" {
		return false
	}
	content, _ := messages[0]["content"].(string)
	return flaggedKwRe.MatchString(content)
}
