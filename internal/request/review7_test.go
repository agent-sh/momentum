package request

// Tests from the seventh review of the requests change. Synthetic data only.

import (
	"strings"
	"testing"
)

// The guards that keep what made nothing from reading as made, whether or not a stored call ran them: a dry run
// among short options, a redirect of standard output to a file, a commit's dry run, a fetch's ref lines.
func TestTheGuardsHold(t *testing.T) {
	notes := "last run:\n[main 9f8e7d6] An older commit from another repo\n 2 files changed"
	newBranch := "To github.com:o/r.git\n * [new branch]      feat -> feat"
	cases := []struct{ name, input, output string }{
		{"push -nv", `{"command":"git push -nv origin feat"}`, newBranch},
		{"push -un", `{"command":"git push -un origin feat"}`, newBranch},
		{"push -fn", `{"command":"git push -fn origin feat"}`, newBranch},
		{"a commit sent to a file with >|", `{"command":"git commit -am Fix >| c.log; cat notes.txt"}`, notes},
		{"a commit appended to a file with &>>", `{"command":"git commit -am Fix &>> build.log; cat notes.txt"}`, notes},
		{"commit --dry-run", `{"command":"git commit --dry-run -am Fix; cat notes.txt"}`, notes},
		{"a remote update's ref line beside a push a tail cut", `{"command":"git remote update && git push origin main 2>&1 | tail -n 1"}`,
			"   1111111..2222222  main -> origin/main"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := artifactsAs("Bash", c.input, c.output, false); got != "" {
				t.Errorf("got %q, want none", got)
			}
		})
	}
}

// A quiet push prints no ref line: with no To line, the ref lines another command printed are not its.
func TestAQuietPushPrintsNoRefLine(t *testing.T) {
	got := artifactsAs("Bash", `{"command":"git submodule update --remote && git push -q"}`,
		"From github.com:o/sub\n   1111111..2222222  main -> origin/main\nSubmodule path 'sub': checked out '2222222'", false)
	if got != "" {
		t.Errorf("got %q", got)
	}
}

// A push's remote is its first argument that is no option nor an option's value.
func TestAPushsRemoteIsNoOptionsValue(t *testing.T) {
	for _, c := range []struct{ input, want string }{
		{`{"command":"git push -o ci.skip origin main 2>&1 | tail -n 1"}`, "push pushed main -> main (1111111..2222222) to origin"},
		{`{"command":"git push upstream main 2>&1 | tail -n 1"}`, "push pushed main -> main (1111111..2222222) to upstream"},
	} {
		if got := artifactsAs("Bash", c.input, "1111111..2222222 main -> main", false); got != c.want {
			t.Errorf("%s: got %q", c.input, got)
		}
	}
}

// gh commands that print pull requests' addresses beside gh pr create, behind gh's own flags too: the call makes
// no pull request, since which address is the one it made is not known.
func TestGhCommandsThatPrintAddresses(t *testing.T) {
	out := "https://github.com/o/r/pull/8\nhttps://github.com/o/r/pull/5\n"
	for _, cmd := range []string{
		"gh pr create --fill && gh -R o/r pr list --json url -q '.[].url'",
		"gh pr create --fill; gh --repo o/r pr view 5 --json url -q .url",
		"gh pr create --fill && gh pr edit 5 --add-label x",
		"gh pr create --fill && gh pr comment 5 --body ok",
		"gh pr create --fill && gh pr ready 5",
		"gh pr create --fill && gh search prs --author @me --json url --jq '.[].url'",
	} {
		if got := artifactsAs("Bash", `{"command":`+jsonString(cmd)+`}`, out, false); got != "" {
			t.Errorf("%s: got %q", cmd, got)
		}
	}
	if got := artifactsAs("Bash", `{"command":"gh -R o/r pr create --fill"}`, "https://github.com/o/r/pull/8\n", false); got != "pr pull request https://github.com/o/r/pull/8" {
		t.Errorf("gh -R o/r pr create: got %q", got)
	}
}

// Codex's code mode: each exec_command's result is read against its own command, not one output of them all.
func TestACodeModeResultIsReadAgainstItsOwnCommand(t *testing.T) {
	cases := []struct{ name, input, output, want string }{
		{"gh pr edit, then gh pr create still running",
			`text(await tools.exec_command({cmd:"gh pr edit 137 --body-file b.md"})); text(await tools.exec_command({cmd:"git push -u origin x && gh pr create --fill"}))`,
			`Script completed Output: {"exit_code":0,"output":"https://github.com/o/r/pull/137\n"} {"session_id":5,"output":""}`, ""},
		{"a script that edits through python, then gh pr create",
			`text(await tools.exec_command({cmd:"python3 edit.py 137"})); text(await tools.exec_command({cmd:"gh pr create --fill"}))`,
			`Script completed Output: {"exit_code":0,"output":"https://github.com/o/r/pull/137\n"} {"exit_code":0,"output":"https://github.com/o/r/pull/140\n"}`,
			"pr pull request https://github.com/o/r/pull/140"},
		{"a commit, then a push that failed",
			`text(await tools.exec_command({cmd:"git commit -am Fix"})); text(await tools.exec_command({cmd:"git push origin main"}))`,
			`Script completed Output: {"exit_code":0,"output":"[main 1a2b3c4] Fix\n 1 file changed\n"} {"exit_code":1,"output":"To github.com:o/r.git\n   1111111..2222222  main -> main\n ! [rejected]        topic -> topic (fetch first)\n"}`,
			"commit commit 1a2b3c4 on main: Fix"},
		{"more results than commands",
			`text(await tools.exec_command({cmd:"gh pr create --fill"})); text(await tools.write_stdin({session_id:5,chars:""}))`,
			`Script completed Output: {"exit_code":0,"output":"https://github.com/o/r/pull/9\n"} {"exit_code":0,"output":"https://github.com/o/r/pull/3\n"}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := artifactsAs("exec", c.input, c.output, false); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// The shells whose -c script is read, and wrappers' shapes.
func TestTheShellsAndWrappersRead(t *testing.T) {
	push := "To github.com:o/r.git\n   1111111..2222222  main -> main"
	for _, cmd := range []string{
		"sh -c 'git push origin main'",
		"zsh -c 'git push origin main'",
		"dash -c 'git push origin main'",
		"env - git push origin main",
	} {
		got := artifactsAs("Bash", `{"command":`+jsonString(cmd)+`}`, push, false)
		if !strings.Contains(got, "pushed main -> main") {
			t.Errorf("%s: got %q", cmd, got)
		}
	}
}
