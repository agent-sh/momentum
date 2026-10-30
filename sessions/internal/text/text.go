// Package text holds the small text helpers every session tracker shares: one-line clipping, the first
// non-blank value, the readable part of a tool call, and the text of a message's content blocks.
package text

import (
	"encoding/json"
	"strings"
)

// OneLine folds runs of whitespace into single spaces and clips the result to n runes, marking a cut with an
// ellipsis.
func OneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if n <= 0 {
		return ""
	}
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// FirstNonEmpty returns the first value that is not blank, unchanged, or "".
func FirstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if strings.TrimSpace(x) != "" {
			return x
		}
	}
	return ""
}

// Str returns v when it is a string, else "".
func Str(v any) string {
	s, _ := v.(string)
	return s
}

// Describe returns the readable part of a tool call's input: its description, command, path, pattern, prompt and
// so on, in that order. A question names its first question, and a plan approval says so.
func Describe(name string, in map[string]any) string {
	if name != "AskUserQuestion" && name != "ExitPlanMode" {
		for _, k := range []string{"description", "command", "file_path", "pattern", "prompt", "url", "query", "skill", "message", "to"} {
			if v := Str(in[k]); v != "" {
				return v
			}
		}
	}
	switch name {
	case "AskUserQuestion":
		if qs, ok := in["questions"].([]any); ok && len(qs) > 0 {
			if q, ok := qs[0].(map[string]any); ok {
				return Str(q["question"])
			}
		}
	case "ExitPlanMode":
		return "plan ready for your approval"
	}
	return ""
}

// BlocksText returns the text of message content that is either a string or a list of blocks with a text field,
// the blocks joined by spaces.
func BlocksText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var bs []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &bs) != nil {
		return ""
	}
	parts := make([]string, 0, len(bs))
	for _, b := range bs {
		if b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, " ")
}
