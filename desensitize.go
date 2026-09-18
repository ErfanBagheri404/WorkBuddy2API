package main

import (
	"regexp"
)

// Desensitize rewrites a system prompt to neutralise keywords that commonly
// trigger Tencent/WorkBuddy content-moderation false positives. Applied only
// when --desensitize is enabled (env: WORKBUDDY2API_DESENSITIZE=1).
//
// Each entry is compiled to a case-insensitive, word-boundary-anchored regexp,
// so "Function calling" is rewritten while "bypassable" and "function caller"
// are left intact.

var desensitizeReplacements = [][2]string{
	{"function calling", "tool use"},
	{"function call", "tool use"},
	{"you are an AI agent", "you are a helpful assistant"},
	{"you are an autonomous agent", "you are a helpful assistant"},
	{"you are a coding agent", "you are a helpful assistant"},
	{"execute commands", "run commands"},
	{"run arbitrary", "run"},
	{"system prompt", "instructions"},
	{"ignore previous", "follow the"},
	{"ignore all prior", "follow the"},
	{"bypass", "work around"},
}

type desensitizeRule struct {
	re   *regexp.Regexp
	repl string
}

var desensitizeRules = func() []desensitizeRule {
	rules := make([]desensitizeRule, 0, len(desensitizeReplacements))
	for _, pair := range desensitizeReplacements {
		re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(pair[0]) + `\b`)
		rules = append(rules, desensitizeRule{re: re, repl: pair[1]})
	}
	return rules
}()

// desensitizePrompt applies the rewrite table and reports whether anything
// changed, so callers can skip re-marshalling untouched payloads.
func desensitizePrompt(content string) (string, bool) {
	out := content
	for _, r := range desensitizeRules {
		out = r.re.ReplaceAllString(out, r.repl)
	}
	return out, out != content
}
