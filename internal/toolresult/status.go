// Package toolresult reads explicit failure signals before a result is clipped.
// An error word in ordinary output is not a failure signal.
package toolresult

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

var (
	exit       = regexp.MustCompile(`(?i)(?:^|\s)(?:(?:process|command) exited with code|exit code:?)\s*(-?\d+)\b`)
	jsonExit   = regexp.MustCompile(`"(?:exit_code|exitCode)"\s*:\s*(-?\d+)`)
	jsonStatus = regexp.MustCompile(`"status"\s*:\s*"(?:error|failed)"`)
	jsonFlag   = regexp.MustCompile(`"(?:is_error|isError)"\s*:\s*true|"(?:success|ok)"\s*:\s*false`)
	jsonError  = regexp.MustCompile(`"error"\s*:\s*"(?:[^"\\]|\\.)`)
	refusal    = regexp.MustCompile(`(?i)^(?:<tool_use_error>)?(?:` +
		`pretooluse:\S+ hook error:|[a-z][a-z0-9-]*(?:[ -]guard|-gate)\s*:|` +
		`this agent is isolated in the worktree|permission for this command was denied|blocked:|` +
		`permission to use \S+ .*has been denied|permission for this (?:action|tool use) (?:has been|was) denied|` +
		`command failed due to container (?:sandboxdenied|sandboxerror)|` +
		`sandbox(?:denied|error):|patch rejected:|approval policy is |` +
		`the (?:command|tool call|action) (?:was |is )?(?:rejected|denied|blocked) by (?:the )?(?:sandbox|policy|hook)|` +
		`tool (?:execution|call) timed out|command timed out|execution timed out|` +
		`error:|tool error:|tool call error:|apply_patch verification failed:|` +
		`failed to (?:find expected lines|read file to update)|invalid patch:|error parsing function call:|` +
		`unsupported custom tool call|script failed\b)`)
	cancelled = regexp.MustCompile(`(?i)^(?:<tool_use_error>)?(?:` +
		`the user doesn't want to (?:take this action|proceed with this tool use)|` +
		`cancelled: parallel tool call|permission for this tool use was denied\. the tool use was rejected|` +
		`aborted by user|(?:error:\s*)?user (?:rejected|cancelled|canceled|denied)|` +
		`tool (?:execution|call) cancel(?:led|ed)(?: by user)?|` +
		`(?:tool call|command) was cancel(?:led|ed) by (?:the )?user)`)
)

func Cancelled(text string) bool { return cancelled.MatchString(strings.TrimSpace(text)) }

// Failed reads harness headers, tool JSON and explicit refusals. flagged is the
// native reader's error flag. A cancellation is excluded even when flagged.
func Failed(harness, tool, text string, flagged bool) bool {
	text = strings.TrimSpace(text)
	if Cancelled(text) {
		return false
	}
	if flagged {
		return true
	}
	if (harness == "hermes" || IsShell(tool)) && strings.HasPrefix(text, "{") {
		return EnvelopeFailed(text)
	}
	// Only a header before Output names the harness's exit, not a command's
	// own output. Kimi and Grok may put the code first without Output.
	header := text
	if i := strings.Index(strings.ToLower(header), "output:"); i >= 0 {
		header = header[:i]
	}
	if m := exit.FindStringSubmatch(header); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n != 0
	}
	return refusal.MatchString(text)
}

// EnvelopeFailed reads a known native result wrapper, not file or tool content.
func EnvelopeFailed(text string) bool {
	if strings.HasPrefix(text, "{") {
		var obj map[string]json.RawMessage
		if json.Unmarshal([]byte(text), &obj) == nil {
			for _, key := range []string{"is_error", "isError"} {
				if string(obj[key]) == "true" {
					return true
				}
			}
			for _, key := range []string{"error", "errors"} {
				v := string(obj[key])
				if v != "" && v != "null" && v != `""` && v != "[]" && v != "{}" && v != "false" {
					return true
				}
			}
			if string(obj["status"]) == `"error"` || string(obj["status"]) == `"failed"` {
				return true
			}
			if string(obj["success"]) == "false" || string(obj["ok"]) == "false" {
				return true
			}
		}
		// A store summary can end before the JSON does.
		if m := jsonExit.FindStringSubmatch(text); m != nil {
			return m[1] != "0"
		}
		if jsonStatus.MatchString(text) || jsonFlag.MatchString(text) || jsonError.MatchString(text) {
			return true
		}
	}
	return false
}

func IsShell(tool string) bool {
	switch strings.ToLower(tool) {
	case "bash", "bash_output", "powershell", "exec", "exec_command", "shell", "local_shell", "write_stdin", "terminal", "execute_code":
		return true
	}
	return false
}
