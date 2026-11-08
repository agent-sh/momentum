package detect

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// A failed command whose message says no file is at a path it names in quotes is keyed by that path as a whole one,
// cleaned and with the home folded to ~, not by its words: Python's errno, ls's and the like's quoted name before the
// mark, Node's code and its message, in any harness's shell. A relative path is read in the directory the command
// ran in; one whose directory is not known, an empty one and one the store cut short key nothing. A message that
// names no quoted path, two paths, or a program a wrapper could not find keeps its words.
func TestACommandsMissIsKeyedByThePathItNames(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	const py = `{"command":"python3 run.py"}`
	for _, c := range []struct{ harness, tool, text, call, cwd, want string }{
		// each form, by the path it names
		{"claude", "Bash", "Exit code 1 FileNotFoundError: [Errno 2] No such file or directory: '/w/data/in.csv'", py, "", "miss:exec:/w/data/in.csv"},
		{"claude", "Bash", "Exit code 1 Traceback (most recent call last): File \"run.py\", line 3, in <module> open('config') FileNotFoundError: [Errno 2] No such file or directory: 'config'",
			py, "/w/a", "miss:exec:/w/a/config"},
		{"claude", "Bash", `Exit code 1 FileNotFoundError: [Errno 2] No such file or directory: "/w/it's.md"`, py, "", "miss:exec:/w/it's.md"},
		{"claude", "Bash", "Exit code 2 ls: cannot access '/w/a.txt': No such file or directory", `{"command":"ls /w/a.txt"}`, "", "miss:exec:/w/a.txt"},
		{"claude", "Bash", "Exit code 2 ls: cannot access 'a.txt': No such file or directory", `{"command":"ls a.txt"}`, "/w", "miss:exec:/w/a.txt"},
		{"claude", "Bash", "Exit code 2 ls: cannot access ‘build’: No such file or directory", `{"command":"ls build"}`, "/w", "miss:exec:/w/build"},
		{"claude", "Bash", "Exit code 2 ls: cannot access `build': No such file or directory", `{"command":"ls build"}`, "/w", "miss:exec:/w/build"},
		{"claude", "Bash", "Exit code 2 python3: can't open file '/w/run.py': [Errno 2] No such file or directory", py, "/w", "miss:exec:/w/run.py"},
		{"claude", "Bash", "Exit code 1 tail: cannot open '/w/x.log' for reading: No such file or directory", `{"command":"tail /w/x.log"}`, "", "miss:exec:/w/x.log"},
		{"claude", "Bash", `Exit code 23 rsync: [sender] link_stat "/w/src" failed: No such file or directory (2)`, `{"command":"rsync -a /w/src /w/dst"}`, "", "miss:exec:/w/src"},
		{"claude", "Bash", "Exit code 128 fatal: cannot change to 'repo': No such file or directory", `{"command":"git -C repo status"}`, "/w", "miss:exec:/w/repo"},
		{"claude", "Bash", "Exit code 1 Error: ENOENT: no such file or directory, open '/srv/app/a.json' at Object.openSync (node:fs:1)", `{"command":"node x.js"}`, "", "miss:exec:/srv/app/a.json"},
		{"claude", "Bash", "Exit code 1 Error: ENOENT: no such file or directory, scandir 'out'", `{"command":"node x.js"}`, "/w", "miss:exec:/w/out"},
		{"claude", "Bash", `Exit code 1 "/w/target/asan": No such file or directory (os error 2)`, `{"command":"cargo x"}`, "", "miss:exec:/w/target/asan"},
		// the path's cleaning: . and .., the home, a program's wrapper that names one by its path
		{"claude", "Bash", "Exit code 1 FileNotFoundError: [Errno 2] No such file or directory: '/w/./b/../a.txt'", py, "", "miss:exec:/w/a.txt"},
		{"claude", "Bash", "Exit code 1 FileNotFoundError: [Errno 2] No such file or directory: '/home/u/p/x.json'", py, "", "miss:exec:~/p/x.json"},
		{"claude", "Bash", "Exit code 1 FileNotFoundError: [Errno 2] No such file or directory: '~/p/x.json'", py, "", "miss:exec:~/p/x.json"},
		{"claude", "Bash", "Exit code 2 ls: cannot access '../a.txt': No such file or directory", `{"command":"ls ../a.txt"}`, "/w/b", "miss:exec:/w/a.txt"},
		{"claude", "Bash", "Exit code 127 env: './run.sh': No such file or directory", `{"command":"env ./run.sh"}`, "/w", "miss:exec:/w/run.sh"},
		// each harness's shell, and the directory its call names
		{"codex", "exec_command", `{"stdout":"","stderr":"ls: cannot access '/w/a': No such file or directory","exit_code":2}`, `{"cmd":"ls /w/a"}`, "", "miss:exec:/w/a"},
		{"codex", "exec_command", "Chunk ID: 1a Wall time: 0.1 seconds Process exited with code 2 Original token count: 3 Output: ls: cannot access 'a': No such file or directory",
			`{"cmd":"ls a","workdir":"/w/c"}`, "/w", "miss:exec:/w/c/a"},
		{"codex", "exec_command", "Process exited with code 2 Output: ls: cannot access 'a': No such file or directory", `{"cmd":"ls a"}`, "/w", "miss:exec:/w/a"},
		{"hermes", "terminal", `{"output": "ls: cannot access 'a': No such file or directory", "exit_code": 2, "error": null}`, `{"command":"ls a","workdir":"/w/h"}`, "/w", "miss:exec:/w/h/a"},
		{"hermes", "terminal", `{"output": "ls: cannot access '/w/a': No such file or directory", "exit_code": 2, "error": null}`, `{"command":"ls /w/a"}`, "", "miss:exec:/w/a"},
		{"kimi", "Bash", "ls: cannot access 'a': No such file or directory", `{"command":"ls a","cwd":"/w/k"}`, "/w", "miss:exec:/w/k/a"},
		{"pi", "bash", "ls: cannot access 'a': No such file or directory", `{"command":"ls a"}`, "/w/p", "miss:exec:/w/p/a"},
		// no key: an empty path, one the store cut short, a relative one whose directory is not known
		{"claude", "Bash", "Exit code 1 FileNotFoundError: [Errno 2] No such file or directory: ''", py, "/w", ""},
		{"claude", "Bash", "Exit code 2 ls: cannot access '': No such file or directory", `{"command":"ls \"$X\""}`, "/w", ""},
		{"claude", "Bash", "Exit code 1 FileNotFoundError: [Errno 2] No such file or directory: '/w/a-very-long-dir/sub…", py, "/w", ""},
		{"claude", "Bash", "Exit code 2 ls: cannot access 'a.txt': No such file or directory", `{"command":"ls a.txt"}`, "", ""},
		{"claude", "Bash", "Exit code 2 ls: cannot access 'a.txt': No such file or directory", `{"command":"ls a.txt"}`, "w", ""},
		{"claude", "Bash", "Exit code 2 ls: cannot access 'a.txt': No such file or directory", "", "/w", ""},
		{"claude", "Bash", "Exit code 2 ls: cannot access 'a.txt': No such file or directory", `{"command":"ls a.txt && echo a long command the store cut`, "/w", ""},
		{"claude", "Bash", "Exit code 2 ls: cannot access 'a.txt': No such file or directory", `{"command":"cd /x && ls a.txt"}`, "/w", ""},
		{"claude", "Bash", "Exit code 2 ls: cannot access 'a.txt': No such file or directory", `{"command":"(pushd /x; ls a.txt)"}`, "/w", ""},
		{"claude", "Bash", "Exit code 2 ls: cannot access 'a.txt': No such file or directory", `{"command":"ssh h 'ls a.txt'"}`, "/w", ""},
		{"claude", "Bash", "Exit code 1 FileNotFoundError: [Errno 2] No such file or directory: 'a.txt'", `{"command":"cat > x.py <<'EOF'\nopen('a.txt')\nEOF\ncd /x && python3 x.py"}`, "/w", ""},
		{"codex", "exec_command", "Process exited with code 2 Output: ls: cannot access 'a': No such file or directory", `{"cmd":"ls a","yield_time_ms":1000,"max_out`, "/w", ""},
		{"hermes", "terminal", `{"output": "ls: cannot access 'a': No such file or directory", "exit_code": 2, "error": null}`, `{"command":"ls a"}`, "/w", ""},
		// what keeps its words: a path not quoted, two paths, a program a wrapper looked for on the PATH, a quote too far
		// from the mark or opened in a word
		{"claude", "Bash", "Exit code 1 cat: /w/a.txt: No such file or directory", `{"command":"cat /w/a.txt"}`, "/w", "exec:cat: P: no such file or directory"},
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
	// the mistakes log keeps no call: a whole path is keyed, a relative one is not
	if k, ok := MistakeKey("tool-error", "Bash", "Bash: Exit code 2 ls: cannot access '/w/a.txt': No such file or directory"); !ok || k.String() != "miss:exec:/w/a.txt" {
		t.Errorf("a logged miss: %q, %v", k.String(), ok)
	}
	if k, ok := MistakeKey("tool-error", "Bash", "Bash: Exit code 2 ls: cannot access 'a.txt': No such file or directory"); ok {
		t.Errorf("a logged relative miss: %q", k.String())
	}
}

// Two commands that miss two paths are two failures, where their words were one; the same path missed again, in
// another session too, recurs, under its own title.
func TestACommandsMissRecursOnlyByItsPath(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	ctx := context.Background()
	n := 0
	missing := func(session, p, cwd string, at time.Time) []store.Event {
		n++
		id := "bash-" + string(rune('a'+n))
		c := call(session, "Bash", id, `{"command":"python3 run.py"}`, at)
		c.Cwd = cwd
		return []store.Event{c, result(session, "Bash", id, "Exit code 1 FileNotFoundError: [Errno 2] No such file or directory: '"+p+"'", at.Add(time.Second))}
	}
	st := testStore(t)
	put(t, st, slices.Concat(missing("s1", "/w/a.json", "/w", t0), missing("s1", "/w/b.json", "/w", t0.Add(time.Hour)),
		missing("s2", "c.json", "/w", t0.Add(2*time.Hour)))...)
	if fs, err := (Failures{}).Detect(ctx, st, week()); err != nil || len(fs) != 0 {
		t.Fatalf("three paths: %v, %v", signatures(fs), err)
	}
	fs, err := Failures{Recur: Recur{Events: 1}}.Detect(ctx, st, week())
	if got, want := signatures(fs), []string{"failures/v2:miss:exec:/w/a.json 1", "failures/v2:miss:exec:/w/b.json 1", "failures/v2:miss:exec:/w/c.json 1"}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("three paths at a bar of one: %v, %v, want %v", got, err, want)
	}

	st = testStore(t)
	put(t, st, slices.Concat(missing("s1", "/home/u/p/x.json", "/w", t0), missing("s1", "x.json", "/home/u/p", t0.Add(time.Hour)),
		missing("s2", "/home/u/p/./x.json", "/w", t0.Add(2*time.Hour)), missing("s3", "x.json", "/w", t0))...)
	outs, _, err := Run(ctx, st, []Detector{Failures{}}, week(), "test", false)
	if err != nil || len(outs) != 1 || len(outs[0].Filed) != 1 || outs[0].Filed[0].Observed != store.Opened {
		t.Fatalf("one path: %+v, %v", outs, err)
	}
	if c := outs[0].Filed[0].Case; !slices.Equal(c.Signatures, []string{"failures/v2:miss:exec:~/p/x.json"}) || c.Evidence != 3 ||
		c.Title != "A command keeps missing the same path: ~/p/x.json" {
		t.Fatalf("case %+v", c)
	}

	// no write clears a command's miss, so none is awaited: a miss in the window's last hour counts at once
	w := week()
	st = testStore(t)
	put(t, st, slices.Concat(missing("s1", "/w/a.json", "/w", t0), missing("s2", "/w/a.json", "/w", t0.Add(time.Hour)),
		missing("s2", "/w/a.json", "/w", w.Until.Add(-10*time.Minute)))...)
	if fs, err := (Failures{}).Detect(ctx, st, w); err != nil || !slices.Equal(signatures(fs), []string{"failures/v2:miss:exec:/w/a.json 3"}) {
		t.Fatalf("a miss at the window's end: %v, %v", signatures(fs), err)
	}
}
