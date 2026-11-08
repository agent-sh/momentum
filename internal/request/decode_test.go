package request

import (
	"strings"
	"testing"
)

// Each harness keeps a call's input and its result's output in its own shape; the decoder reads each one as the
// harness stores it, the store's cut included. Synthetic content in each harness's real shape.
func TestEachHarnesssStoredShapeIsDecoded(t *testing.T) {
	cases := []struct {
		name, harness, tool, input, output string
		commands, out, paths               string
	}{
		{"Claude Code Bash", "claude", "Bash", `{"command":"git commit -m \"Fix it\"","description":"Commit the fix"}`,
			"[main 1a2b3c4] Fix it 1 file changed", `git commit -m "Fix it"`, "[main 1a2b3c4] Fix it 1 file changed", ""},
		{"Claude Code Write", "claude", "Write", `{"file_path":"/work/a.go","content":"package a"}`, "File created.", "", "File created.", "/work/a.go"},
		{"Codex exec_command", "codex", "exec_command", `{"cmd":"git push origin main","workdir":"/work/x","yield_time_ms":1000}`,
			"Chunk ID: 1f Wall time: 0.4 seconds Process exited with code 0 Output: To github.com:example/x.git 1111111..2222222 main -> main",
			"git push origin main", "Chunk ID: 1f Wall time: 0.4 seconds Process exited with code 0 Output: To github.com:example/x.git 1111111..2222222 main -> main", ""},
		{"Codex shell argv", "codex", "local_shell", `{"command":["bash","-lc","git commit -am 'Fix it'"],"workdir":"/work/x"}`,
			"[main 1a2b3c4] Fix it", "git commit -am 'Fix it'", "[main 1a2b3c4] Fix it", ""},
		{"Codex exec code mode", "codex", "exec", `text(await tools.exec_command({cmd:"git add -A; git commit -m 'Fix it'","workdir":"/work/x"}))`,
			`Script completed Wall time 1.5 seconds Output: {"chunk_id":"15bcbe","exit_code":0,"output":"[main 1a2b3c4] Fix it\n 1 file changed"}`,
			"git add -A; git commit -m 'Fix it'", "[main 1a2b3c4] Fix it\n 1 file changed", ""},
		{"Codex exec code mode, two calls, a quoted key and single quotes", "codex", "exec",
			`const a = await tools.exec_command({"cmd": 'git push origin main'}); const b = await tools.exec_command({command: "gh pr create --fill"});`,
			`Script completed Output: {"output":"To github.com:example/x.git\n   1111111..2222222  main -> main"} {"output":"https://github.com/example/x/pull/7\n"}`,
			"git push origin main | gh pr create --fill", "To github.com:example/x.git\n   1111111..2222222  main -> main\nhttps://github.com/example/x/pull/7\n", ""},
		// the token the cut ends is dropped: it may be shorter than it was
		{"Codex exec code mode, cut inside the command", "codex", "exec", `text(await tools.exec_command({cmd:"git -C /work/x commit -m 'Build the port for…`,
			`Script completed Output: {"exit_code":0,"output":"[main (root-commit) 8aebac8] Build the port\n 7 files c…`,
			"git -C /work/x commit -m 'Build the port ", "[main (root-commit) 8aebac8] Build the port\n 7 files ", ""},
		{"Hermes terminal", "hermes", "terminal", `{"command": "git push origin main", "timeout": 120}`,
			`{"output": "ok\nTo github.com:example/x.git\n   1111111..2222222  main -> main", "exit_code": 0, "error": null}`,
			"git push origin main", "ok\nTo github.com:example/x.git\n   1111111..2222222  main -> main", ""},
		{"Hermes write_file", "hermes", "write_file", `{"path": "/work/a.py", "content": "x = \"{\\\"path\\\": 1}\""}`, `{"bytes_written": 5}`, "", `{"bytes_written": 5}`, "/work/a.py"},
		{"Hermes patch, replace", "hermes", "patch", `{"mode": "replace", "path": "/work/b.py", "old_string": "a", "new_string": "b"}`, `{"success": true}`, "", `{"success": true}`, "/work/b.py"},
		{"Hermes patch, a patch", "hermes", "patch", `{"mode": "patch", "patch": "*** Begin Patch\n*** Update File: /work/c.py\n@@\n-a\n+b\n*** End Patch"}`, `{"success": true}`, "", `{"success": true}`, "/work/c.py"},
		{"opencode bash", "opencode", "bash", `{"command":"git commit -m fix","description":"Commit"}`, "[main 1a2b3c4] fix", "git commit -m fix", "[main 1a2b3c4] fix", ""},
		{"opencode patch", "opencode", "patch", `{"patchText":"*** Begin Patch\n*** Add File: /work/d.go\n+package d\n*** Update File: /work/e.go\n*** End Patch"}`, "Success.", "", "Success.", "/work/d.go | /work/e.go"},
		{"opencode edit", "opencode", "edit", `{"filePath":"/work/f.ts","oldString":"a","newString":"b"}`, "Edit applied.", "", "Edit applied.", "/work/f.ts"},
		{"pi bash", "pi", "bash", `{"command":"git commit -am fix","timeout":60}`, "[main 1a2b3c4] fix", "git commit -am fix", "[main 1a2b3c4] fix", ""},
		{"Kimi Shell", "kimi", "Shell", `{"command":"git push origin main"}`, "To github.com:example/x.git 1111111..2222222 main -> main", "git push origin main",
			"To github.com:example/x.git 1111111..2222222 main -> main", ""},
		{"Kimi Write", "kimi", "Write", `{"path":"/work/g.py","content":"x = 1"}`, "Wrote 5 bytes.", "", "Wrote 5 bytes.", "/work/g.py"},
		{"a writer cut before its path", "kimi", "Edit", `{"new_string":"a long text the store cut before the path…`, "Edited.", "", "Edited.", ""},
		{"Codex apply_patch, raw", "codex", "apply_patch", "*** Begin Patch *** Update File: src/h.go @@ -a +b *** End Patch", "Success.", "", "Success.", "src/h.go"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Decode(c.harness, c.tool, c.input, c.output)
			if got := strings.Join(d.Commands, " | "); got != c.commands {
				t.Errorf("commands: got %q, want %q", got, c.commands)
			}
			if d.Output != c.out {
				t.Errorf("output: got %q, want %q", d.Output, c.out)
			}
			if got := strings.Join(d.Paths, " | "); got != c.paths {
				t.Errorf("paths: got %q, want %q", got, c.paths)
			}
		})
	}
}

// A shell command is parsed as a shell parses it: every program a line runs, inside lists, pipelines, loops,
// conditions, subshells and substitutions, past assignments and wrappers, and a cut line as far as it goes.
func TestEveryProgramALineRunsIsRead(t *testing.T) {
	cases := []struct{ cmd, want string }{
		{"git commit -m x", "git commit -m x"},
		{"cd /x && git add -A && git commit -m 'a b; c'", "cd /x | git add -A | git commit -m a b; c"},
		{"FOO=1 timeout 120 nice -n 5 env -u HOME BAR=2 nohup git push origin main", "git push origin main"},
		{"timeout -s KILL 30s sudo -u bot command git push", "git push"},
		{"for r in a b; do gh pr create -R example/$r --fill; done", "gh pr create -R example/\x00 --fill"},
		{"if git diff --quiet; then echo same; else git commit -am x; fi", "git diff --quiet | echo same | git commit -am x"},
		{"echo $(git rev-parse HEAD) | tee out.txt", "echo \x00 | git rev-parse HEAD | tee out.txt"},
		{"(cd sub && git push) 2>&1 | head -n 2", "cd sub | git push | head -n 2"},
		{`bash -lc "git commit -m 'x' && git push"`, "git commit -m x | git push"},
		{"command -v git", ""},
		{"env -u", ""},
		{"nice -n", ""},
		{"timeout 5", ""},
		{"echo git commit", "echo git commit"},
		{"git commit -m 'Build the port for", "git commit -m Build the port for"},
		{"git add a; git commit -m \"unclosed && git push", "git add a | git commit -m unclosed && git push"},
	}
	for _, c := range cases {
		var got []string
		for _, inv := range invocations(c.cmd, 0) {
			got = append(got, strings.TrimSpace(inv.prog+" "+strings.Join(inv.args, " ")))
		}
		if g := strings.Join(got, " | "); g != c.want {
			t.Errorf("%q: got %q, want %q", c.cmd, g, c.want)
		}
	}
}
