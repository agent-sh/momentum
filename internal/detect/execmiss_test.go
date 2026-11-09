package detect

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// A failed command whose message says no file is at a path it names in quotes is keyed by that path, not by its
// words: Python's errno, ls's and the like's quoted name before the mark, Node's code and its message, in any
// harness's shell. The path is cleaned; a whole one has the home folded to ~, and one under the directory the call
// ran in (its input's workdir or cwd, else the call's own) is keyed by its part below that directory; a relative one
// is keyed as the message spells it, whatever the command did before. An empty path and one the store cut short key
// nothing. A message that names two paths, or a program that another (a wrapper, Python's subprocess, strace, Deno)
// could not find keeps its words, and so does one from a command that may have run on another host that its command
// line does not name.
func TestACommandsMissIsKeyedByThePathItNames(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	const py = `{"command":"python3 run.py"}`
	for _, c := range []struct{ harness, tool, text, call, cwd, want string }{
		// each form, by the path it names
		{"claude", "Bash", "Exit code 1 FileNotFoundError: [Errno 2] No such file or directory: '/w/data/in.csv'", py, "", "miss:exec:/w/data/in.csv"},
		{"claude", "Bash", "Exit code 1 Traceback (most recent call last): File \"run.py\", line 3, in <module> open('config') FileNotFoundError: [Errno 2] No such file or directory: 'config'",
			py, "/w/a", "miss:exec:config"},
		{"claude", "Bash", `Exit code 1 FileNotFoundError: [Errno 2] No such file or directory: "/w/it's.md"`, py, "", "miss:exec:/w/it's.md"},
		{"claude", "Bash", "Exit code 2 ls: cannot access '/w/a.txt': No such file or directory", `{"command":"ls /w/a.txt"}`, "/x", "miss:exec:/w/a.txt"},
		{"claude", "Bash", "Exit code 2 ls: cannot access ‘build’: No such file or directory", `{"command":"ls build"}`, "/w", "miss:exec:build"},
		{"claude", "Bash", "Exit code 2 ls: cannot access `build': No such file or directory", `{"command":"ls build"}`, "/w", "miss:exec:build"},
		{"claude", "Bash", "Exit code 2 python3: can't open file '/w/run.py': [Errno 2] No such file or directory", py, "/x", "miss:exec:/w/run.py"},
		{"claude", "Bash", "Exit code 1 tail: cannot open '/w/x.log' for reading: No such file or directory", `{"command":"tail /w/x.log"}`, "", "miss:exec:/w/x.log"},
		{"claude", "Bash", `Exit code 23 rsync: [sender] link_stat "/w/src" failed: No such file or directory (2)`, `{"command":"rsync -a /w/src /w/dst"}`, "", "miss:exec:/w/src"},
		{"claude", "Bash", "Exit code 128 fatal: cannot change to 'repo': No such file or directory", `{"command":"git -C repo status"}`, "/w", "miss:exec:repo"},
		{"claude", "Bash", "Exit code 1 Error: ENOENT: no such file or directory, open '/srv/app/a.json' at Object.openSync (node:fs:1)", `{"command":"node x.js"}`, "", "miss:exec:/srv/app/a.json"},
		{"claude", "Bash", "Exit code 1 Error: ENOENT: no such file or directory, scandir 'out'", `{"command":"node x.js"}`, "/w", "miss:exec:out"},
		{"claude", "Bash", `Exit code 1 "/w/target/asan": No such file or directory (os error 2)`, `{"command":"cargo x"}`, "", "miss:exec:/w/target/asan"},
		// ls's escaped quote is read; quotes run together otherwise ($'\n') are not
		{"claude", "Bash", `Exit code 2 ls: cannot access 'it'\''s.md': No such file or directory`, `{"command":"ls x"}`, "/w", "miss:exec:it's.md"},
		{"claude", "Bash", `Exit code 2 ls: cannot access '/w/a'\''b'\''c': No such file or directory`, `{"command":"ls x"}`, "", "miss:exec:/w/a'b'c"},
		{"claude", "Bash", `Exit code 2 ls: cannot access 'a'$'\n''b': No such file or directory`, `{"command":"ls x"}`, "/w", ""},
		// the path's cleaning: . and .., the home
		{"claude", "Bash", "Exit code 1 FileNotFoundError: [Errno 2] No such file or directory: '/w/./b/../a.txt'", py, "", "miss:exec:/w/a.txt"},
		{"claude", "Bash", "Exit code 1 FileNotFoundError: [Errno 2] No such file or directory: '/home/u/p/x.json'", py, "", "miss:exec:~/p/x.json"},
		{"claude", "Bash", "Exit code 2 ls: cannot access './a/./b.txt': No such file or directory", `{"command":"ls x"}`, "/w", "miss:exec:a/b.txt"},
		{"claude", "Bash", "Exit code 2 ls: cannot access '../a.txt': No such file or directory", `{"command":"ls ../a.txt"}`, "/w/b", "miss:exec:../a.txt"},
		// a relative path as spelled, wherever the command ran: a ~ in a program's message is its directory's, since the
		// shell expands the home's before the program sees it
		{"claude", "Bash", "Exit code 1 FileNotFoundError: [Errno 2] No such file or directory: '~/p/x.json'", py, "/home/u/p", "miss:exec:~/p/x.json"},
		{"claude", "Bash", "Exit code 1 FileNotFoundError: [Errno 2] No such file or directory: '~/p/x.json'", py, "/home/u", "miss:exec:~/p/x.json"},
		{"claude", "Bash", "Exit code 127 env: './run.sh': No such file or directory", `{"command":"env ./run.sh"}`, "/w", "miss:exec:run.sh"},
		{"claude", "Bash", "Exit code 2 ls: cannot access 'a.txt': No such file or directory", `{"command":"ls a.txt"}`, "", "miss:exec:a.txt"},
		{"claude", "Bash", "Exit code 2 ls: cannot access 'a.txt': No such file or directory", `{"command":"ls a.txt"}`, "w", "miss:exec:a.txt"},
		{"claude", "Bash", "Exit code 2 ls: cannot access 'a.txt': No such file or directory", "", "/w", "miss:exec:a.txt"},
		{"claude", "Bash", "Exit code 2 ls: cannot access 'a.txt': No such file or directory", `{"command":"ls a.txt && echo a long command the store cut`, "/w", "miss:exec:a.txt"},
		{"claude", "Bash", "Exit code 2 ls: cannot access 'a.txt': No such file or directory", `{"command":"cd /x && ls a.txt"}`, "/w", "miss:exec:a.txt"},
		{"claude", "Bash", "Exit code 2 ls: cannot access 'a.txt': No such file or directory", `{"command":"(pushd /x; ls a.txt)"}`, "/w", "miss:exec:a.txt"},
		{"claude", "Bash", "Exit code 2 ls: cannot access 'a.txt': No such file or directory", `{"command":"popd && ls a.txt"}`, "/w", "miss:exec:a.txt"},
		{"claude", "Bash", "Exit code 1 FileNotFoundError: [Errno 2] No such file or directory: 'a.txt'", `{"command":"cat > x.py <<'EOF'\nopen('a.txt')\nEOF\ncd /x && python3 x.py"}`, "/w", "miss:exec:a.txt"},
		{"claude", "Bash", "Exit code 1 cat: '.out/result.json': No such file or directory", `{"command":"cat > run.sh <<'EOF'\nset -e\nmkdir -p .out\nprintf x > .out/a`, "/w", "miss:exec:.out/result.json"},
		{"codex", "exec_command", "Process exited with code 2 Output: ls: cannot access 'a': No such file or directory", `{"cmd":"ls a","yield_time_ms":1000,"max_out`, "/w", "miss:exec:a"},
		{"hermes", "terminal", `{"output": "ls: cannot access 'a': No such file or directory", "exit_code": 2, "error": null}`, `{"command":"ls a"}`, "/w", "miss:exec:a"},
		// a whole path under the directory the call ran in, by its part below it: the one its input names, else its own
		{"claude", "Bash", "Exit code 2 ls: cannot access '/w/wt1/scripts/c.py': No such file or directory", `{"command":"ls x"}`, "/w/wt1", "miss:exec:scripts/c.py"},
		{"codex", "exec_command", `{"stdout":"","stderr":"ls: cannot access '/w/c/a': No such file or directory","exit_code":2}`, `{"cmd":"ls /w/c/a","workdir":"/w/c"}`, "/w", "miss:exec:a"},
		{"codex", "exec_command", `{"stdout":"","stderr":"ls: cannot access '/w/b': No such file or directory","exit_code":2}`, `{"cmd":"ls /w/b","workdir":"/w/c"}`, "/w", "miss:exec:/w/b"},
		{"codex", "exec_command", "Chunk ID: 1a Wall time: 0.1 seconds Process exited with code 2 Original token count: 3 Output: ls: cannot access 'a': No such file or directory",
			`{"cmd":"ls a","workdir":"/w/c"}`, "/w", "miss:exec:a"},
		// no directory the store can vouch for keeps the whole path: a Codex or Kimi call cut before its directory's
		// field, Hermes's terminal with none
		{"codex", "exec_command", "Process exited with code 2 Output: ls: cannot access '/w/a': No such file or directory", `{"cmd":"ls /w/a","yield_time_ms":1000,"max_out`, "/w", "miss:exec:/w/a"},
		{"hermes", "terminal", `{"output": "ls: cannot access '/w/h/a': No such file or directory", "exit_code": 2, "error": null}`, `{"command":"ls /w/h/a"}`, "/w/h", "miss:exec:/w/h/a"},
		{"kimi", "Bash", "ls: cannot access '/w/k/a': No such file or directory", `{"command":"ls /w/k/a","timeout":60,"des`, "/w/k", "miss:exec:/w/k/a"},
		{"opencode", "shell", "ls: cannot access '/w/o/a': No such file or directory", `{"command":"ls /w/o/a","workdir":"/w/o"}`, "/w", "miss:exec:a"},
		{"hermes", "terminal", `{"output": "ls: cannot access '/w/h/a': No such file or directory", "exit_code": 2, "error": null}`, `{"command":"ls /w/h/a","workdir":"/w/h"}`, "/w", "miss:exec:a"},
		{"hermes", "terminal", `{"output": "ls: cannot access '/w/a': No such file or directory", "exit_code": 2, "error": null}`, `{"command":"ls /w/a"}`, "", "miss:exec:/w/a"},
		{"kimi", "Bash", "ls: cannot access '/w/k/a': No such file or directory", `{"command":"ls /w/k/a","cwd":"/w/k"}`, "/w", "miss:exec:a"},
		{"pi", "bash", "ls: cannot access '/w/p/a': No such file or directory", `{"command":"ls /w/p/a"}`, "/w/p", "miss:exec:a"},
		{"claude", "Bash", "Exit code 2 ls: cannot access '/home/u/.ssh/id_x': No such file or directory", `{"command":"ls ~/.ssh/id_x"}`, "/w", "miss:exec:~/.ssh/id_x"},
		{"claude", "Bash", "Exit code 1 ssh-keygen: '/w/k': No such file or directory", `{"command":"ssh-keygen -l -f /w/k"}`, "/x", "miss:exec:/w/k"},
		{"claude", "Bash", "Exit code 2 ls: cannot access '/w/scpx/a': No such file or directory", `{"command":"rsync -a /w/src /w/dst; ls /w/scpx/a"}`, "/x", "miss:exec:/w/scpx/a"},
		{"hermes", "execute_code", `{"status": "error", "output": "FileNotFoundError: [Errno 2] No such file or directory: '/w/in.csv'"}`, `{"code":"open('/w/in.csv')"}`, "", "miss:exec:/w/in.csv"},
		{"hermes", "execute_code", `{"status": "error", "output": "FileNotFoundError: [Errno 2] No such file or directory: '/w/in.csv'"}`, `{"code":"import os\nprint(1)\nopen('/w/in.cs…`, "", "miss:exec:/w/in.csv"},
		// not below it: the directory itself, a name that only starts like it, the root, the home and above it
		{"claude", "Bash", "Exit code 128 fatal: cannot change to '/w/a': No such file or directory", `{"command":"git -C /w/a status"}`, "/w/a", "miss:exec:/w/a"},
		{"claude", "Bash", "Exit code 2 ls: cannot access '/w/ab/c': No such file or directory", `{"command":"ls x"}`, "/w/a", "miss:exec:/w/ab/c"},
		{"claude", "Bash", "Exit code 2 ls: cannot access '/w/a': No such file or directory", `{"command":"ls x"}`, "/", "miss:exec:/w/a"},
		{"claude", "Bash", "Exit code 1 FileNotFoundError: [Errno 2] No such file or directory: '/home/u/p/x.json'", py, "/home/u", "miss:exec:~/p/x.json"},
		{"claude", "Bash", "Exit code 1 FileNotFoundError: [Errno 2] No such file or directory: '/home/u/p/x.json'", py, "/home", "miss:exec:~/p/x.json"},
		// no key: an empty path, one the store cut short
		{"claude", "Bash", "Exit code 1 FileNotFoundError: [Errno 2] No such file or directory: ''", py, "/w", ""},
		{"claude", "Bash", "Exit code 2 ls: cannot access '': No such file or directory", `{"command":"ls \"$X\""}`, "/w", ""},
		{"claude", "Bash", "Exit code 1 FileNotFoundError: [Errno 2] No such file or directory: '/w/a-very-long-dir/sub…", py, "/w", ""},
		// what keeps its words: two paths, a program another program looked for on the PATH, a command that may have run
		// on a host it does not name, a quote too far from the mark or opened in a word
		{"claude", "Bash", "Exit code 1 FileNotFoundError: [Errno 2] No such file or directory: '/w/a' -> '/w/b'", py, "/w",
			"exec:filenotfounderror: [errno N] no such file or directory: Q"},
		{"claude", "Bash", "Exit code 1 Error: ENOENT: no such file or directory, rename '/w/a' -> '/w/b'", `{"command":"node x.js"}`, "/w", "exec:error: enoent: no such file or directory"},
		{"claude", "Bash", "Exit code 1 mv: cannot move '/w/a' to '/w/b/c': No such file or directory", `{"command":"mv /w/a /w/b/c"}`, "/w",
			"exec:mv: cannot move Q to Q: no such file or directory"},
		{"claude", "Bash", "Exit code 1 ln: failed to create symbolic link '/w/a' -> '/w/b': No such file or directory", `{"command":"ln -s /w/b /w/a"}`, "/w",
			"exec:ln: failed to create symbolic link Q -> Q: no such file or directory"},
		{"codex", "exec_command", "Process exited with code 127 Output: env: ‘cargo’: No such file or directory", `{"cmd":"env cargo build"}`, "/w", "exec:env: Q: no such file or directory"},
		{"claude", "Bash", "Exit code 127 /usr/bin/env: 'python3\r': No such file or directory", `{"command":"./run"}`, "/w", "exec:no such file or directory"},
		{"claude", "Bash", "Exit code 127 timeout: failed to run command 'gsutil': No such file or directory", `{"command":"timeout 5 gsutil ls"}`, "/w",
			"exec:timeout: failed to run command Q: no such file or directory"},
		{"claude", "Bash", "Exit code 1 Traceback (most recent call last): File \"x.py\", line 2, in <module> File \"/usr/lib/python3.12/subprocess.py\", line 1955, in _execute_child " +
			"raise child_exception_type(errno_num, err_msg, err_filename) FileNotFoundError: [Errno 2] No such file or directory: 'cargo'", py, "/w",
			"exec:filenotfounderror: [errno N] no such file or directory: Q"},
		{"claude", "Bash", "Exit code 1 Traceback (most recent call last): File \"/usr/lib/python3.12/subprocess.py\", line 1955, in _execute_child " +
			"raise child_exception_type(errno_num, err_msg, err_filename) FileNotFoundError: [Errno 2] No such file or directory: './bin/tool'", py, "/w", "miss:exec:bin/tool"},
		{"claude", "Bash", "Exit code 1 strace: Can't stat 'foo': No such file or directory", `{"command":"strace -f foo"}`, "/w", "exec:strace: can't stat Q: no such file or directory"},
		{"claude", "Bash", "Exit code 1 error: Uncaught (in promise) NotFound: Failed to spawn 'foo': No such file or directory (os error 2)", `{"command":"deno run x.ts"}`, "/w",
			"exec:notfound: failed to spawn Q: no such file or directory"},
		{"claude", "Bash", `Exit code 11 rsync: [Receiver] mkdir "/root/x" failed: No such file or directory (2)`, `{"command":"rsync -az -e \"ssh -o BatchMode=yes\" /w/x /root/x"}`, "/w",
			"exec:rsync: [receiver] mkdir Q failed: no such file or directory"},
		{"claude", "Bash", `Exit code 11 rsync: [Receiver] mkdir "/root/x" failed: No such file or directory (2)`, `{"command":"rsync -avze sshpass /w/x /root/x"}`, "/w",
			"exec:rsync: [receiver] mkdir Q failed: no such file or directory"},
		{"claude", "Bash", `Exit code 11 rsync: [Receiver] mkdir "/root/x" failed: No such file or directory (2)`, `{"command":"rsync --rsh=sshpass /w/x /root/x"}`, "/w",
			"exec:rsync: [receiver] mkdir Q failed: no such file or directory"},
		{"claude", "Bash", `Exit code 11 rsync: [Receiver] mkdir "/root/x" failed: No such file or directory (2)`, `{"command":"cd /w && rsync -a x \"root@box:/root/x/\""}`, "/w",
			"exec:rsync: [receiver] mkdir Q failed: no such file or directory"},
		{"claude", "Bash", `Exit code 1 sftp: cannot access '/root/x': No such file or directory`, `{"command":"echo get /root/x | sftp -b - box"}`, "/w",
			"exec:sftp: cannot access Q: no such file or directory"},
		{"claude", "Bash", "Exit code 128 fatal: cannot change to '/workspace/a': No such file or directory", `{"command":"GIT_SSH_COMMAND=\"ssh -i k\" git -C /workspace/a fetch"}`, "/w",
			"exec:fatal: cannot change to Q: no such file or directory"},
		{"codex", "exec", "Process exited with code 2 Output: ls: cannot access '/workspace/a': No such file or directory", `text(await tools.exec_command({cmd:"ssh box ls /workspace/a"}))`, "/w",
			"exec:ls: cannot access Q: no such file or directory"},
		{"codex", "write_stdin", "Process exited with code 2 Output: ls: cannot access '/workspace/a': No such file or directory", `{"session_id":3,"chars":"ls /workspace/a\n"}`, "/w",
			"exec:ls: cannot access Q: no such file or directory"},
		{"claude", "bash_output", "ls: cannot access '/workspace/a': No such file or directory", `{"bash_id":"b1"}`, "/w", "exec:ls: cannot access Q: no such file or directory"},
		{"claude", "Bash", "Exit code 2 ls: cannot access '/workspace/a': No such file or directory", `{"command":"cat > x <<'EOF'\nls\nEOF\nssh box sh < x"}`, "/w",
			"exec:ls: cannot access Q: no such file or directory"},
		{"claude", "Bash", "Exit code 1 waiting for 'Allow once' clip-2.mp4: No such file or directory", `{"command":"run"}`, "/w", "exec:clip-N.mpN: no such file or directory"},
		{"claude", "Bash", "Exit code 1 x 'a.txt' one two three four: No such file or directory", `{"command":"x"}`, "/w", "exec:four: no such file or directory"},
		{"claude", "Bash", "Exit code 1 it can't open: No such file or directory", `{"command":"x"}`, "/w", "exec:open: no such file or directory"},
		{"claude", "Bash", "Exit code 1 it can't x': No such file or directory", `{"command":"x"}`, "/w", "exec:no such file or directory"},
	} {
		k, ok := failureKey(c.harness, c.tool, c.text, store.Call{Input: c.call, Cwd: c.cwd})
		got := ""
		if ok {
			got = k.String()
		}
		if got != c.want {
			t.Errorf("%s %s %q, call %q in %q:\n got %q\nwant %q", c.harness, c.tool, c.text, c.call, c.cwd, got, c.want)
		}
	}
	// the mistakes log keeps no call: a path is keyed as its message spells it
	if k, ok := MistakeKey("tool-error", "Bash", "Bash: Exit code 2 ls: cannot access '/w/a.txt': No such file or directory"); !ok || k.String() != "miss:exec:/w/a.txt" {
		t.Errorf("a logged miss: %q, %v", k.String(), ok)
	}
	if k, ok := MistakeKey("tool-error", "Bash", "Bash: Exit code 2 ls: cannot access 'a.txt': No such file or directory"); !ok || k.String() != "miss:exec:a.txt" {
		t.Errorf("a logged relative miss: %q, %v", k.String(), ok)
	}
}

// A program that names the path it found no file at unquoted, "PROG: PATH: No such file or directory", is keyed by
// that path as a quoted one is: rg, cat, wc, grep, cut, sha256sum, sed's "can't read", ugrep's "warning", the
// shell's cd, bash's script and its line's redirection, with the same directory, remote and cut rules. What is no
// path keeps its words: an option or a glob in its place, a path with a space, the program bash could not run by
// its name, and a setup failure's message: a bad interpreter, a wrapper's own, a deleted working directory, two
// paths.
func TestACommandsUnquotedMissIsKeyedByItsPath(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	for _, c := range []struct{ harness, tool, text, call, cwd, want string }{
		// each program, by the path it names
		{"claude", "Bash", "Exit code 2 rg: src/a.rs: No such file or directory (os error 2)", `{"command":"rg -n x src/a.rs"}`, "/w", "miss:exec:src/a.rs"},
		{"claude", "Bash", "Exit code 1 cat: /x/a.txt: No such file or directory", `{"command":"cat /x/a.txt"}`, "/w", "miss:exec:/x/a.txt"},
		{"claude", "Bash", "Exit code 1 wc: logs/b.log: No such file or directory", `{"command":"wc -l logs/b.log"}`, "/w", "miss:exec:logs/b.log"},
		{"claude", "Bash", "Exit code 2 grep: ./config.json: No such file or directory", `{"command":"grep x ./config.json"}`, "/w", "miss:exec:config.json"},
		{"claude", "Bash", "Exit code 2 /usr/bin/grep: crates/a.rs: No such file or directory", `{"command":"grep x crates/a.rs"}`, "/w", "miss:exec:crates/a.rs"},
		{"claude", "Bash", "Exit code 1 cut: /x/d.txt: No such file or directory", `{"command":"cut -f1 /x/d.txt"}`, "/w", "miss:exec:/x/d.txt"},
		{"claude", "Bash", "Exit code 1 sha256sum: dist/a.tar.gz: No such file or directory", `{"command":"sha256sum dist/a.tar.gz"}`, "/w", "miss:exec:dist/a.tar.gz"},
		{"claude", "Bash", "Exit code 2 sed: can't read README.md: No such file or directory", `{"command":"sed -n 1,5p README.md"}`, "/w", "miss:exec:README.md"},
		{"claude", "Bash", "Exit code 1 ugrep: warning: .config/x.json: No such file or directory", `{"command":"grep x .config/x.json"}`, "/w", "miss:exec:.config/x.json"},
		{"claude", "Bash", "Exit code 1 bash: line 1: cd: review-1: No such file or directory", `{"command":"cd review-1 && ls"}`, "/w", "miss:exec:review-1"},
		{"hermes", "terminal", `{"output": "/usr/bin/bash: line 5: cd: /x/agent: No such file or directory\n===", "exit_code": 1, "error": null}`, `{"command":"cd /x/agent; echo ==="}`, "/w",
			"miss:exec:/x/agent"},
		{"codex", "exec_command", "Process exited with code 127 Output: bash: scripts/check.sh: No such file or directory", `{"cmd":"bash scripts/check.sh"}`, "/w", "miss:exec:scripts/check.sh"},
		{"claude", "Bash", "Exit code 127 bash: run.sh: No such file or directory", `{"command":"bash run.sh"}`, "/w", "miss:exec:run.sh"},
		{"claude", "Bash", "Exit code 1 /bin/bash: line 3: /x/out/a.log: No such file or directory", `{"command":"run > /x/out/a.log"}`, "/w", "miss:exec:/x/out/a.log"},
		{"claude", "Bash", "Exit code 1 bash: line 1: in.txt: No such file or directory", `{"command":"wc -l < in.txt"}`, "/w", "miss:exec:in.txt"},
		// the quoted form's rules: the call's directory stripped, the home folded, a command the store cut, the mistakes
		// log's, a host
		{"claude", "Bash", "Exit code 1 cat: /w/wt1/scripts/c.py: No such file or directory", `{"command":"cat scripts/c.py"}`, "/w/wt1", "miss:exec:scripts/c.py"},
		{"claude", "Bash", "Exit code 1 cat: /home/u/p/x.json: No such file or directory", `{"command":"cat ~/p/x.json"}`, "/w", "miss:exec:~/p/x.json"},
		{"claude", "Bash", "Exit code 1 cat: a.txt: No such file or directory", `{"command":"cat a.txt && echo a long command the store cut`, "/w", "miss:exec:a.txt"},
		{"claude", "Bash", "Exit code 1 cat: /x/a.txt: No such file or directory", "", "", "miss:exec:/x/a.txt"},
		{"claude", "Bash", "Exit code 1 cat: /r/a: No such file or directory", `{"command":"ssh box cat /r/a"}`, "/w", "miss:exec:box:/r/a"},
		{"claude", "Bash", "Exit code 1 scp: /r/x.log: No such file or directory", `{"command":"scp box:/r/x.log /w/"}`, "/w", "miss:exec:box:/r/x.log"},
		// words: a host the command line does not name, two scp hosts
		{"claude", "Bash", "Exit code 1 cat: /r/a: No such file or directory", `{"command":"ssh $H cat /r/a"}`, "/w", "exec:cat: P: no such file or directory"},
		{"claude", "Bash", "Exit code 1 scp: /r/x.log: No such file or directory", `{"command":"scp a:/r/x.log b:/r/"}`, "/w", "exec:scp: P: no such file or directory"},
		// words: no path in its place, a path with a space
		{"claude", "Bash", "Exit code 2 rg: -g: No such file or directory (os error 2)", `{"command":"rg x -- -g"}`, "/w", "exec:rg: -g: no such file or directory"},
		{"claude", "Bash", "Exit code 2 rg: *.sh: No such file or directory (os error 2)", `{"command":"rg x '*.sh'"}`, "/w", "exec:rg: *.sh: no such file or directory"},
		{"claude", "Bash", "Exit code 2 sed: can't read my notes.md: No such file or directory", `{"command":"sed -n 1p my notes.md"}`, "/w", "exec:sed: can't read my P: no such file or directory"},
		// words: a program bash could not run by its name, the call's or of a call that names none
		{"claude", "Bash", "Exit code 127 bash: foo: No such file or directory", `{"command":"foo --x"}`, "/w", "exec:foo: no such file or directory"},
		{"claude", "Bash", "Exit code 127 bash: line 1: foo: No such file or directory", `{"command":"cd /x && sudo foo"}`, "/w", "exec:foo: no such file or directory"},
		{"claude", "Bash", "Exit code 127 bash: foo: No such file or directory", "", "/w", "exec:foo: no such file or directory"},
		// words: a setup failure's message, a bad interpreter, a wrapper's own, a deleted working directory, two paths
		{"claude", "Bash", "Exit code 126 bash: ./x: /bin/foo: bad interpreter: No such file or directory", `{"command":"./x"}`, "/w", "exec:interpreter: no such file or directory"},
		{"claude", "Bash", "Exit code 127 env: foo: No such file or directory", `{"command":"env foo"}`, "/w", "exec:foo: no such file or directory"},
		{"claude", "Bash", "Exit code 127 timeout: foo: No such file or directory", `{"command":"timeout 5 foo"}`, "/w", "exec:foo: no such file or directory"},
		{"claude", "Bash", "Exit code 127 nohup: foo: No such file or directory", `{"command":"nohup foo"}`, "/w", "exec:foo: no such file or directory"},
		{"claude", "Bash", "Exit code 1 shell-init: error retrieving current directory: getcwd: cannot access parent directories: No such file or directory",
			`{"command":"ls"}`, "/w", "exec:directories: no such file or directory"},
		{"claude", "Bash", "Exit code 1 cd: error retrieving current directory: getcwd: cannot access parent directories: No such file or directory",
			`{"command":"cd ."}`, "/w", "exec:cd: error retrieving current directory: getcwd: cannot access parent directories: no such file or directory"},
		{"claude", "Bash", "Exit code 1 mv: rename /x/a to /x/b: No such file or directory", `{"command":"mv /x/a /x/b"}`, "/w", "exec:mv: rename P to P: no such file or directory"},
	} {
		k, ok := failureKey(c.harness, c.tool, c.text, store.Call{Input: c.call, Cwd: c.cwd})
		got := ""
		if ok {
			got = k.String()
		}
		if got != c.want {
			t.Errorf("%s %s %q, call %q in %q:\n got %q\nwant %q", c.harness, c.tool, c.text, c.call, c.cwd, got, c.want)
		}
	}
	if k, ok := MistakeKey("tool-error", "Bash", "Bash: Exit code 1 sed: can't read a.txt: No such file or directory"); !ok || k.String() != "miss:exec:a.txt" {
		t.Errorf("a logged unquoted miss: %q, %v", k.String(), ok)
	}
}

// A miss of a command that ran on another host is keyed by that host, as its command line names it, and the path: a
// plain ssh's host past its options, scp's or rsync's host:path operand when the message is that host's side. The
// path is cleaned, but no directory of this host's is stripped from it. A command line that does not name the host
// keeps its words.
func TestARemoteMissIsKeyedByItsHostAndPath(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	const ls = "Exit code 2 ls: cannot access '/w/a': No such file or directory"
	const words = "exec:ls: cannot access Q: no such file or directory"
	for _, c := range []struct{ harness, tool, text, call, want string }{
		// ssh: the first word past its options and their values, its user@ off
		{"claude", "Bash", ls, `{"command":"ssh box 'ls /w/a'"}`, "miss:exec:box:/w/a"},
		{"claude", "Bash", "Exit code 2 ls: cannot access 'a.txt': No such file or directory", `{"command":"ssh h 'ls a.txt'"}`, "miss:exec:h:a.txt"},
		{"claude", "Bash", ls, `{"command":"ssh -p 2222 -i ~/k -o BatchMode=yes u@box 'ls /w/a'"}`, "miss:exec:box:/w/a"},
		{"claude", "Bash", ls, `{"command":"ssh -nT -p22 -J jump -l u box ls -l /w/a"}`, "miss:exec:box:/w/a"},
		{"claude", "Bash", ls, `{"command":"ssh -o \"ProxyCommand=ssh -W %h:%p j\" box ls /w/a 2>&1"}`, "miss:exec:box:/w/a"},
		{"claude", "Bash", "Exit code 2 python3: can't open file '/w/run.py': [Errno 2] No such file or directory",
			`{"command":"L=1 timeout 30 ssh -n box 'python3 /w/run.py'"}`, "miss:exec:box:/w/run.py"},
		{"codex", "exec_command", "Process exited with code 2 Output: " + ls[len("Exit code 2 "):], `{"cmd":"ssh box ls /w/a","workdir":"/w"}`, "miss:exec:box:/w/a"},
		{"hermes", "terminal", `{"output": "ls: cannot access '/w/a': No such file or directory", "exit_code": 2, "error": null}`, `{"command":"ssh box ls /w/a"}`, "miss:exec:box:/w/a"},
		// a command the store cut is read up to the cut
		{"claude", "Bash", ls, `{"command":"ssh box 'cd /w && for f in a b; do ls /w/$f; done; python3 -c \"import json, sys`, "miss:exec:box:/w/a"},
		// the path cleaned, the home folded, no directory stripped, an empty one no key
		{"claude", "Bash", "Exit code 2 ls: cannot access '/w/./b/../a': No such file or directory", `{"command":"ssh box ls x"}`, "miss:exec:box:/w/a"},
		{"claude", "Bash", "Exit code 2 ls: cannot access '/home/u/p/x': No such file or directory", `{"command":"ssh box ls x"}`, "miss:exec:box:~/p/x"},
		{"claude", "Bash", "Exit code 2 ls: cannot access '': No such file or directory", `{"command":"ssh box ls \"$X\""}`, ""},
		// scp and rsync by their host operand, when the message is that host's
		{"claude", "Bash", `Exit code 1 scp: dest open "/root/x/y": No such file or directory`, `{"command":"scp -q /w/y root@box:/root/x/"}`, "miss:exec:box:/root/x/y"},
		{"claude", "Bash", `Exit code 1 scp: dest open "/r/y": No such file or directory`, `{"command":"scp -P 22 -o X=1 /w/y box:/r/"}`, "miss:exec:box:/r/y"},
		{"claude", "Bash", `Exit code 11 rsync: [Receiver] mkdir "/root/x" failed: No such file or directory (2)`, `{"command":"rsync -a -e $S /w/x box:path/"}`, "miss:exec:box:/root/x"},
		{"claude", "Bash", `Exit code 23 rsync: [generator] recv_generator: mkdir "/r/x" failed: No such file or directory (2)`, `{"command":"rsync -a --exclude c /w/x u@box:/r/"}`, "miss:exec:box:/r/x"},
		{"claude", "Bash", `Exit code 23 rsync: [sender] change_dir "/r/src" failed: No such file or directory (2)`, `{"command":"rsync -a box:/r/src /w/dst"}`, "miss:exec:box:/r/src"},
		// an ssh option that takes a value is skipped, and so is a value rsync's short options take
		{"claude", "Bash", ls, `{"command":"ssh -e none box ls /w/a"}`, "miss:exec:box:/w/a"},
		{"claude", "Bash", `Exit code 11 rsync: [Receiver] mkdir "/r/x" failed: No such file or directory (2)`, `{"command":"rsync -e ssh -avz /w/x box:/r/"}`, "miss:exec:box:/r/x"},
		// words: a host from a variable, a here-document or a file on ssh's stdin, a pipe, a second command, ssh in a
		// script, an ssh with no command, one whose command goes on to a third host
		{"claude", "Bash", ls, `{"command":"ssh $H 'ls /w/a'"}`, words},
		{"claude", "Bash", ls, `{"command":"ssh \"${H}\" 'ls /w/a'"}`, words},
		{"claude", "Bash", ls, `{"command":"ssh box bash <<'EOF'\nls /w/a\nEOF"}`, words},
		{"claude", "Bash", ls, `{"command":"ssh box sh < x.sh"}`, words},
		{"claude", "Bash", ls, `{"command":"ssh box 'ls /w/a' | tail -1"}`, words},
		{"claude", "Bash", ls, `{"command":"ssh box 'ls /w/b'; ls /w/a"}`, words},
		{"claude", "Bash", ls, `{"command":"ssh box 'ls /w/b'\nls /w/a"}`, words},
		{"claude", "Bash", ls, `{"command":"ssh box ls $(cat /w/list)"}`, words},
		{"claude", "Bash", ls, `{"command":"bash -c \"ssh box ls /w/a\""}`, words},
		{"claude", "Bash", ls, `{"command":"ssh -o BatchMode=yes box"}`, words},
		{"claude", "Bash", `Exit code 11 rsync: [Receiver] mkdir "/w/a" failed: No such file or directory (2)`, `{"command":"ssh box 'rsync -a /w/x other:/w/a'"}`,
			"exec:failed: no such file or directory"},
		{"claude", "Bash", ls, `{"command":"ssh -J jump box 'ssh other ls /w/a'"}`, words},
		// words: scp's or rsync's message of this host's side, or of no side, and two hosts
		{"claude", "Bash", `Exit code 1 scp: stat local "/w/y": No such file or directory`, `{"command":"scp /w/y box:/r/"}`, "exec:scp: stat local Q: no such file or directory"},
		{"claude", "Bash", `Exit code 1 scp: dest open "/y": No such file or directory`, `{"command":"scp a:/x b:/"}`, "exec:scp: dest open Q: no such file or directory"},
		{"claude", "Bash", `Exit code 23 rsync: [sender] link_stat "/w/src" failed: No such file or directory (2)`, `{"command":"rsync -a /w/src box:/r/"}`,
			"exec:rsync: [sender] link_stat Q failed: no such file or directory"},
		{"claude", "Bash", `Exit code 11 rsync: [Receiver] mkdir "/w/dst" failed: No such file or directory (2)`, `{"command":"rsync -a box:/r/src /w/dst/"}`,
			"exec:rsync: [receiver] mkdir Q failed: no such file or directory"},
		{"claude", "Bash", `Exit code 23 rsync: change_dir "/r/src" failed: No such file or directory (2)`, `{"command":"rsync -a box:/r/src /w/dst"}`,
			"exec:rsync: change_dir Q failed: no such file or directory"},
		// words: an option after rsync's operands, whose value would read as one, leaves the sides unknown
		{"claude", "Bash", `Exit code 23 rsync: [sender] link_stat "/w/x" failed: No such file or directory (2)`, `{"command":"rsync -a /w/x box:/r/ --exclude .git"}`,
			"exec:rsync: [sender] link_stat Q failed: no such file or directory"},
	} {
		k, ok := failureKey(c.harness, c.tool, c.text, store.Call{Input: c.call, Cwd: "/w"})
		got := ""
		if ok {
			got = k.String()
		}
		if got != c.want {
			t.Errorf("%s %s %q, call %q:\n got %q\nwant %q", c.harness, c.tool, c.text, c.call, got, c.want)
		}
	}
}

// Two commands that miss two paths are two failures, where their words were one; the same path missed again, in
// another session too, recurs, under its own title: a relative one from any directory, after a cd or in a command the
// store cut, one script in three worktrees, by its part below each, and one path on a host, by that host and path.
func TestACommandsMissRecursOnlyByItsPath(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	ctx := context.Background()
	n := 0
	missingIn := func(session, input, p, cwd string, at time.Time) []store.Event {
		n++
		id := "bash-" + string(rune('a'+n))
		c := call(session, "Bash", id, input, at)
		c.Cwd = cwd
		return []store.Event{c, result(session, "Bash", id, "Exit code 1 FileNotFoundError: [Errno 2] No such file or directory: '"+p+"'", at.Add(time.Second))}
	}
	missing := func(session, p, cwd string, at time.Time) []store.Event {
		return missingIn(session, `{"command":"python3 run.py"}`, p, cwd, at)
	}
	st := testStore(t)
	put(t, st, slices.Concat(missing("s1", "/w/a.json", "/x", t0), missing("s1", "/w/b.json", "/x", t0.Add(time.Hour)),
		missing("s2", "c.json", "/x", t0.Add(2*time.Hour)))...)
	if fs, err := (Failures{}).Detect(ctx, st, week()); err != nil || len(fs) != 0 {
		t.Fatalf("three paths: %v, %v", signatures(fs), err)
	}
	fs, err := Failures{Recur: Recur{Events: 1}}.Detect(ctx, st, week())
	if got, want := signatures(fs), []string{"failures/v2:miss:exec:/w/a.json 1", "failures/v2:miss:exec:/w/b.json 1", "failures/v2:miss:exec:c.json 1"}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("three paths at a bar of one: %v, %v, want %v", got, err, want)
	}
	// three misses over ssh, of two paths on one host and one on another, are three keys, where their words were one
	const onBox, onOther = `{"command":"ssh box python3 run.py"}`, `{"command":"ssh other python3 run.py"}`
	st = testStore(t)
	put(t, st, slices.Concat(missingIn("s1", onBox, "/w/a.json", "/x", t0), missingIn("s1", onBox, "/w/b.json", "/x", t0.Add(time.Hour)),
		missingIn("s2", onOther, "/w/a.json", "/x", t0.Add(2*time.Hour)))...)
	if fs, err := (Failures{}).Detect(ctx, st, week()); err != nil || len(fs) != 0 {
		t.Fatalf("three remote paths: %v, %v", signatures(fs), err)
	}
	// three misses of three paths that cat names unquoted are three keys, where their words were one
	st = testStore(t)
	for i, p := range []string{"/w/a.txt", "/w/b.txt", "c.txt"} {
		at := t0.Add(time.Duration(i) * time.Hour)
		id := "cat-" + string(rune('a'+i))
		put(t, st, call("s"+string(rune('1'+i%2)), "Bash", id, `{"command":"cat `+p+`"}`, at),
			result("s"+string(rune('1'+i%2)), "Bash", id, "Exit code 1 cat: "+p+": No such file or directory", at.Add(time.Second)))
	}
	if fs, err := (Failures{}).Detect(ctx, st, week()); err != nil || len(fs) != 0 {
		t.Fatalf("three unquoted paths: %v, %v", signatures(fs), err)
	}

	for _, tc := range []struct {
		name, want string
		events     []store.Event
	}{
		{"the home's", "~/p/x.json", slices.Concat(missing("s1", "/home/u/p/x.json", "/x", t0), missing("s1", "/home/u/p/./x.json", "/x", t0.Add(time.Hour)),
			missing("s2", "/home/u/p/x.json", "/w", t0.Add(2*time.Hour)), missing("s3", "x.json", "/home/u/p", t0))},
		{"a relative one", ".out/result.json", slices.Concat(missing("s1", ".out/result.json", "/w/a", t0),
			missingIn("s1", `{"command":"cd /w/b && python3 run.py"}`, ".out/result.json", "/w", t0.Add(time.Hour)),
			missingIn("s2", `{"command":"cat > run.sh <<'EOF'\nset -e\nmkdir -p .out\nprintf x > .out/a`, "./.out/result.json", "", t0.Add(2*time.Hour)))},
		{"one script in three worktrees", "scripts/c.py", slices.Concat(missing("s1", "/w/wt1/scripts/c.py", "/w/wt1", t0),
			missing("s2", "/w/wt2/scripts/c.py", "/w/wt2", t0.Add(time.Hour)), missing("s3", "/w/wt3/scripts/./c.py", "/w/wt3", t0.Add(2*time.Hour)))},
		{"one path on a host", "box:/w/a.json", slices.Concat(missingIn("s1", onBox, "/w/a.json", "/w", t0),
			missingIn("s1", `{"command":"ssh -p 22 u@box python3 run.py"}`, "/w/./a.json", "/x", t0.Add(time.Hour)),
			missingIn("s2", `{"command":"timeout 30 ssh -n box 'python3 run.py' 2>&1"}`, "/w/a.json", "", t0.Add(2*time.Hour)))},
	} {
		st = testStore(t)
		put(t, st, tc.events...)
		outs, _, err := Run(ctx, st, []Detector{Failures{}}, week(), "test", false)
		if err != nil || len(outs) != 1 || len(outs[0].Filed) != 1 || outs[0].Filed[0].Observed != store.Opened {
			t.Fatalf("%s: %+v, %v", tc.name, outs, err)
		}
		if c := outs[0].Filed[0].Case; !slices.Equal(c.Signatures, []string{"failures/v2:miss:exec:" + tc.want}) || c.Evidence != 3 ||
			c.Title != "A command keeps missing the same path: "+tc.want {
			t.Fatalf("%s: case %+v", tc.name, c)
		}
	}

	// no write clears a command's miss, so none is awaited: a miss in the window's last hour counts at once
	w := week()
	st = testStore(t)
	put(t, st, slices.Concat(missing("s1", "/w/a.json", "/x", t0), missing("s2", "/w/a.json", "/x", t0.Add(time.Hour)),
		missing("s2", "/w/a.json", "/x", w.Until.Add(-10*time.Minute)))...)
	if fs, err := (Failures{}).Detect(ctx, st, w); err != nil || !slices.Equal(signatures(fs), []string{"failures/v2:miss:exec:/w/a.json 3"}) {
		t.Fatalf("a miss at the window's end: %v, %v", signatures(fs), err)
	}
}
