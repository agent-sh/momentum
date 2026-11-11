package detect

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/agent-sh/momentum/internal/store"
)

// hermesMissing is Hermes's read_file of a file that is not there.
func hermesMissing(path string) string {
	return fmt.Sprintf(`{"content": "", "total_lines": 0, "file_size": 0, "truncated": false, "is_binary": false, "is_image": false, "error": "File not found: %s", "not_found": true}`, path)
}

// hermesNoPath is Hermes's search_files of a path that is not there.
func hermesNoPath(path string) string {
	return fmt.Sprintf(`{"total_count": 0, "error": "Path not found: %s"}`, path)
}

// A read tool's own error that no file is at its path, in its harness's words at the error's start, is keyed by the
// read tool and the path its call names: the home folded to ~, . and .. cleaned, nothing else taken out. A read whose
// call names no path has nothing to key it by, and what is no read's miss keeps its words.
func TestANotFoundReadIsKeyedByThePathItMissed(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	for _, c := range []struct{ harness, tool, text, call, want string }{
		// each harness's words for a miss, by the path its call names
		{"claude", "Read", claudeMissing, `{"file_path":"/w/a.md"}`, "miss:read:/w/a.md"},
		{"claude", "Read", "File does not exist. Did you mean b.md?", `{"file_path":"/w/a.md","limit":10}`, "miss:read:/w/a.md"},
		{"kimi", "Read", `"/w/a.md" does not exist.`, `{"line_offset":1,"path":"/w/a.md"}`, "miss:read:/w/a.md"},
		{"hermes", "read_file", hermesMissing("/w/a.md"), `{"path":"/w/a.md","limit":200}`, "miss:read-file:/w/a.md"},
		{"hermes", "read_file", `{"error": "[Errno 2] No such file or directory: '/w/a.md'"}`, `{"path":"/w/a.md"}`, "miss:read-file:/w/a.md"},
		{"hermes", "read_file", `{"error": "[Errno 2] No such file or directory: '/home/u/p/a.md'"}`, `{"path":"~/p/a.md"}`, "miss:read-file:~/p/a.md"},
		{"hermes", "read_file", `{"error": "[Errno 2] No such file or directory: \"/w/it's.md\""}`, `{"path":"/w/./it's.md"}`, "miss:read-file:/w/it's.md"},
		// a relative path's leading .. segments climb to a directory that is not known: the rest is the read's own
		{"hermes", "read_file", `{"error": "[Errno 2] No such file or directory: '/w/a.md'"}`, `{"path":"../a.md"}`, "miss:read-file:/w/a.md"},
		{"hermes", "read_file", `{"error": "[Errno 2] No such file or directory: '/w/a.md'"}`, `{"path":"../x/../../a.md"}`, "miss:read-file:/w/a.md"},
		{"opencode", "read", "Error: File not found: /w/a.md", `{"filePath":"/w/a.md"}`, "miss:read:/w/a.md"},
		{"pi", "read", "ENOENT: no such file or directory, access '/w/docs/x.md'", `{"path":"@/w/docs/x.md"}`, "miss:read:/w/docs/x.md"},
		{"opencode", "read", "File not found: /w/a.md Did you mean one of these? /w/b.md", `{"filePath":"/w/a.md"}`, "miss:read:/w/a.md"},
		{"pi", "read", "ENOENT: no such file or directory, access '/w/a.md'", `{"path":"/w/a.md"}`, "miss:read:/w/a.md"},
		{"grok", "read_file", "Error: /w/a.md does not exist. Note: your current working directory is /w", `{"limit":40,"target_file":"/w/a.md"}`, "miss:read-file:/w/a.md"},
		{"codex", "read_file", codexMissing, `{"file_path":"/w/a.md"}`, "miss:read-file:/w/a.md"},
		// the path as the key holds it: the home folded to ~, . and .. cleaned, nothing else taken out
		{"claude", "Read", claudeMissing, `{"file_path":"/home/u/p/AGENTS.md"}`, "miss:read:~/p/AGENTS.md"},
		{"hermes", "read_file", hermesMissing("/home/u/p/AGENTS.md"), `{"path":"~/p/AGENTS.md"}`, "miss:read-file:~/p/AGENTS.md"},
		{"claude", "Read", claudeMissing, `{"file_path":"/home/u"}`, "miss:read:~"},
		{"claude", "Read", claudeMissing, `{"file_path":"/home/uv/p/AGENTS.md"}`, "miss:read:/home/uv/p/AGENTS.md"},
		{"claude", "Read", claudeMissing, `{"file_path":"/home/u/p/./docs/../AGENTS.md"}`, "miss:read:~/p/AGENTS.md"},
		{"claude", "Read", claudeMissing, `{"file_path":"/home/u/../u/p/AGENTS.md"}`, "miss:read:~/p/AGENTS.md"},
		{"kimi", "Read", `"./a/../CLAUDE.md" does not exist.`, `{"path":"./a/../CLAUDE.md"}`, "miss:read:CLAUDE.md"},
		{"claude", "Read", claudeMissing, `{"file_path":"/w//Run-2 Notes.MD/"}`, "miss:read:/w/Run-2 Notes.MD"},
		{"claude", "Read", claudeMissing, `{"file_path":"/w/a1b2c3d4e5f6/12345.json"}`, "miss:read:/w/a1b2c3d4e5f6/12345.json"},
		// a file named P is its own path's key, never an older key of words (tool:...:file not found: P)
		{"hermes", "read_file", hermesMissing("P"), `{"path":"P"}`, "miss:read-file:P"},
		// Hermes's search_files of a path that is not there, its similar paths after it or not
		{"hermes", "search_files", hermesNoPath("/w/a"), `{"pattern":"x","path":"/w/a"}`, "miss:search-files:/w/a"},
		{"hermes", "search_files", hermesNoPath("/w/a.sh. Similar paths: /w/a.py"), `{"pattern":"x","path":"/w/./a.sh"}`, "miss:search-files:/w/a.sh"},
		{"hermes", "search_files", hermesNoPath("/home/u/p"), `{"pattern":"x","target":"files","path":"~/p"}`, "miss:search-files:~/p"},
		{"hermes", "search_files", hermesNoPath("src"), `{"pattern":"x","path":"src"}`, "miss:search-files:src"},
		{"hermes", "search_files", hermesNoPath("P"), `{"pattern":"x","path":"P"}`, "miss:search-files:P"},
		{"hermes", "search_files", hermesNoPath("/w/a"), `{"pattern":"x"}`, ""},
		// another tool's words for it, or another error of search_files, keep their words
		{"hermes", "read_file", hermesNoPath("/w/a"), `{"path":"/w/a"}`, "tool:read-file:path not found: P"},
		{"hermes", "search_files", hermesMissing("/w/a"), `{"pattern":"x","path":"/w/a"}`, "tool:search-files:file not found: P"},
		{"hermes", "search_files", `{"total_count": 0, "error": "Search failed: Path not found: /w/a"}`, `{"pattern":"x","path":"/w/a"}`, "tool:search-files:search failed: path not found: P"},
		// no path: nothing to key it by
		{"claude", "Read", claudeMissing, "", ""},
		{"claude", "Read", claudeMissing, `{"file_path":"/w/` + strings.Repeat("x", 240) + `…`, ""},
		{"claude", "Read", claudeMissing, `["file_path","/w/a.md"]`, ""},
		{"claude", "Read", claudeMissing, `{"limit":10}`, ""},
		{"claude", "Read", claudeMissing, `{"file_path":""}`, ""},
		{"grok", "read_file", "Error: /w/a.md does not exist.", `{"file":"/w/a.md"}`, ""},
		// what is no read's miss in its harness's words keeps its words
		{"claude", "Read", "<tool_use_error>EACCES: permission denied, open '/w/a.md'</tool_use_error>", `{"file_path":"/w/a.md"}`, "tool:read:eacces: permission denied, open Q"},
		{"claude", "Grep", "<tool_use_error>Path does not exist: /w/a</tool_use_error>", `{"path":"/w/a","pattern":"x"}`, "tool:grep:path does not exist: P"},
		{"claude", "Edit", claudeMissing, `{"file_path":"/w/a.md"}`, "tool:edit:file does not exist. note: your current working directory is P"},
		{"claude", "Read", "<tool_use_error>read-guard: path not found in the allowlist</tool_use_error>", `{"file_path":"/w/a.md"}`, "guard:read-guard:path not found in the allowlist"},
		{"hermes", "read_file", "Tool 'read_file' does not exist. Available tools: terminal, todo", `{"path":"/w/a.md"}`, "tool:read-file:tool Q does not exist"},
		{"hermes", "read_file", `{"content": "", "error": "working directory '/w/gone' does not exist on the active terminal backend (check terminal.cwd or the session cwd). cd: /w/gone: No such file or directory"}`,
			`{"path":"/w/a.md"}`, "tool:read-file:working directory Q does not exist on the active terminal backend (check"},
		// a result the store cut after its error: still Hermes's miss
		{"hermes", "read_file", `{"content": "", "error": "File not found: /w/a.md", "similar_files": ["/w/b`, `{"path":"/w/a.md"}`, "miss:read-file:/w/a.md"},
		{"kimi", "Read", `"/w/a" is not a file.`, `{"path":"/w/a"}`, "tool:read:Q is not a file"},
		{"kimi", "Read", `Failed: "/w/a.md" does not exist.`, `{"path":"/w/a.md"}`, "tool:read:failed: Q does not exist"},
		{"grok", "read_file", "Error: /w/a is a directory, not a file.", `{"target_file":"/w/a"}`, "tool:read-file:error: P is a directory, not a file"},
		// Python's errno for a file that is not the read's: a terminal whose directory is gone names it for any read
		{"hermes", "read_file", `{"error": "[Errno 2] No such file or directory: '/w/gone'"}`, `{"path":"/w/a.md"}`, "tool:read-file:[errno N] no such file or directory: Q"},
		{"hermes", "read_file", `{"error": "[Errno 2] No such file or directory: '/w/gone'"}`, `{"path":"a.md"}`, "tool:read-file:[errno N] no such file or directory: Q"},
		{"hermes", "read_file", `{"error": "[Errno 2] No such file or directory"}`, `{"path":"/w/a.md"}`, "tool:read-file:[errno N] no such file or directory"},
		{"hermes", "read_file", `{"error": "[Errno 2] No such file or directory: '/w/xa.md'"}`, `{"path":"a.md"}`, "tool:read-file:[errno N] no such file or directory: Q"},
		{"hermes", "read_file", `{"error": "[Errno 2] No such file or directory: '/w/c/a.md'"}`, `{"path":"../b/a.md"}`, "tool:read-file:[errno N] no such file or directory: Q"},
		{"hermes", "read_file", `{"error": "[Errno 2] No such file or directory: '/w/a.md'"}`, "", "tool:read-file:[errno N] no such file or directory: Q"},
		{"opencode", "read", "Error: Error: File not found: /w/a.md", `{"filePath":"/w/a.md"}`, "tool:read:error: error: file not found: P"},
		{"pi", "read", "EACCES: permission denied, access '/w/a.md'", `{"path":"/w/a.md"}`, "tool:read:eacces: permission denied, access Q"},
		{"codex", "read_file", "failed to read file: Permission denied (os error 13)", `{"file_path":"/w/a.md"}`, "tool:read-file:failed to read file: permission denied (os error N)"},
		{"codex", "read_file", "File not found: /w/a.md", `{"file_path":"/w/a.md"}`, "tool:read-file:file not found: P"},
		{"claude", "Read", "Error: File does not exist.", `{"file_path":"/w/a.md"}`, "tool:read:error: file does not exist"},
		// a harness's words for a miss count only at the error's start
		{"grok", "read_file", "Read: Error: /w/a.md does not exist.", `{"target_file":"/w/a.md"}`, "tool:read-file:read: error: P does not exist"},
		{"hermes", "read_file", `{"error": "Cannot read 'x': File not found: /w/a.md"}`, `{"path":"/w/a.md"}`, "tool:read-file:cannot read Q: file not found: P"},
		{"pi", "read", "Error: ENOENT: no such file or directory, access '/w/a.md'", `{"path":"/w/a.md"}`, "tool:read:error: enoent: no such file or directory, access Q"},
		{"codex", "read_file", "Error: failed to read file: No such file or directory (os error 2)", `{"file_path":"/w/a.md"}`, "tool:read-file:error: failed to read file: no such file or directory (os error N)"},
	} {
		k, ok := FailureOf(c.harness, c.tool, c.text, c.call)
		got := ""
		if ok {
			got = k.String()
		}
		if got != c.want {
			t.Errorf("%s %s %q, call %q:\n got %q\nwant %q", c.harness, c.tool, c.text, c.call, got, c.want)
		}
	}
}

// A relative path a read missed is read in the directory its call was made in, so one name in two directories is two
// keys; Hermes's read_file resolves it in its terminal's directory, which a cd moves, so there it stays as spelled, as
// it does where no whole directory is known. A path that opens with ~ is the home's.
func TestARelativeMissIsReadInItsCallsDirectory(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	for _, c := range []struct{ harness, tool, text, call, cwd, want string }{
		{"kimi", "Read", `"AGENTS.md" does not exist.`, `{"path":"AGENTS.md"}`, "/w/a", "miss:read:/w/a/AGENTS.md"},
		{"pi", "read", "ENOENT: no such file or directory, access '/w/a/docs/x.md'", `{"path":"./docs/../docs/x.md"}`, "/w/a", "miss:read:/w/a/docs/x.md"},
		{"opencode", "read", "File not found: /home/u/p/x.md", `{"filePath":"x.md"}`, "/home/u/p", "miss:read:~/p/x.md"},
		{"kimi", "Read", `"../AGENTS.md" does not exist.`, `{"path":"../AGENTS.md"}`, "/w/a", "miss:read:/w/AGENTS.md"},
		{"kimi", "Read", `"~/AGENTS.md" does not exist.`, `{"path":"~/AGENTS.md"}`, "/w/a", "miss:read:~/AGENTS.md"},
		{"kimi", "Read", `"/w/b/AGENTS.md" does not exist.`, `{"path":"/w/b/AGENTS.md"}`, "/w/a", "miss:read:/w/b/AGENTS.md"},
		// Hermes's directory, or one the store does not know: the whole path its error names, else the path as spelled
		{"hermes", "read_file", hermesMissing("/w/a/AGENTS.md"), `{"path":"AGENTS.md"}`, "/w/x", "miss:read-file:/w/a/AGENTS.md"},
		{"hermes", "read_file", hermesMissing("/w/b/AGENTS.md"), `{"path":"AGENTS.md"}`, "/w/x", "miss:read-file:/w/b/AGENTS.md"},
		{"hermes", "read_file", hermesMissing("/w/c/AGENTS.md"), `{"path":"AGENTS.md"}`, "/w/x", "miss:read-file:/w/c/AGENTS.md"},
		{"hermes", "read_file", `{"error": "[Errno 2] No such file or directory: '/w/t/x.md'"}`, `{"path":"x.md"}`, "", "miss:read-file:/w/t/x.md"},
		{"hermes", "read_file", hermesMissing("AGENTS.md"), `{"path":"AGENTS.md"}`, "/w/a", "miss:read-file:AGENTS.md"},
		{"opencode", "read", "File not found: /w/o/x.md Did you mean one of these? /w/o/y.md", `{"filePath":"x.md"}`, "", "miss:read:/w/o/x.md"},
		{"opencode", "read", "Error: File not found: /w/o/x.md\n\nDid you mean one of these?\n/w/o/y.md", `{"filePath":"x.md"}`, "", "miss:read:/w/o/x.md"},
		// OpenCode 2.x names the path as its call spells it, in its field path, and may name the folder it found missing
		{"opencode", "read", "File not found: x.md\n\nThe folder /w/o does not exist. The nearest folder that does is /w.", `{"path":"x.md"}`, "", "miss:read:x.md"},
		{"opencode", "read", "File not found: x.md The folder /w/o does not exist. The nearest folder that does is /w. Did you mean one of these? /w/x.md", `{"path":"x.md"}`, "", "miss:read:x.md"},
		{"grok", "read_file", "Error: /w/g/x.md does not exist. Note: your current working directory is /w/g", `{"target_file":"x.md"}`, "", "miss:read-file:/w/g/x.md"},
		{"pi", "read", "ENOENT: no such file or directory, access '/w/p/x.md'", `{"path":"x.md"}`, "", "miss:read:/w/p/x.md"},
		{"pi", "read", "ENOENT: no such file or directory, access '/w/a/docs/x.md'", `{"path":"@docs/x.md"}`, "/w/a", "miss:read:/w/a/docs/x.md"},
		{"kimi", "Read", `"AGENTS.md" does not exist.`, `{"path":"AGENTS.md"}`, "", "miss:read:AGENTS.md"},
		{"kimi", "Read", `"~" does not exist.`, `{"path":"~"}`, "/w/a", "miss:read:~"},
		{"kimi", "Read", `"@a.md" does not exist.`, `{"path":"@a.md"}`, "", "miss:read:@a.md"},
		{"kimi", "Read", `"AGENTS.md" does not exist.`, `{"path":"AGENTS.md"}`, "w/a", "miss:read:AGENTS.md"},
	} {
		k, ok := failureKey(c.harness, c.tool, c.text, store.Call{Input: c.call, Cwd: c.cwd})
		if got := k.String(); !ok || got != c.want {
			t.Errorf("%s %s, call %q in %q: got %q, %v, want %q", c.harness, c.tool, c.call, c.cwd, got, ok, c.want)
		}
	}
	// one name missed in two directories: two keys, neither a finding
	st := testStore(t)
	f := fileTools{harness: "kimi"}
	in := func(dir string, evs []store.Event) []store.Event {
		for i := range evs {
			evs[i].Cwd = dir
		}
		return evs
	}
	put(t, st, slices.Concat(in("/w/a", kimiReadMissing(&f, "s1", "AGENTS.md", t0)), in("/w/a", kimiReadMissing(&f, "s1", "AGENTS.md", t0.Add(time.Hour))),
		in("/w/b", kimiReadMissing(&f, "s2", "AGENTS.md", t0.Add(2*time.Hour))))...)
	fs, err := Failures{Recur: Recur{Events: 1}}.Detect(context.Background(), st, week())
	if got, want := signatures(fs), []string{"failures/v2:miss:read:/w/a/AGENTS.md 2", "failures/v2:miss:read:/w/b/AGENTS.md 1"}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("two directories: %v, %v, want %v", got, err, want)
	}
}

// A path longer than a key's words, or one not plain text on one line, is kept as its end and a short hash of the
// whole: two such paths that end alike stay two keys, and two spellings of one path stay one.
func TestALongMissedPathIsHashed(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	key := func(p string) string {
		t.Helper()
		k, ok := FailureOf("claude", "Read", claudeMissing, fmt.Sprintf(`{"file_path":%q}`, p))
		if !ok {
			t.Fatalf("%q keyed nothing", p)
		}
		return k.Text
	}
	deep := strings.Repeat("d/", 60) + "AGENTS.md"
	a, b := key("/home/u/x/"+deep), key("/home/u/y/"+deep)
	tag := regexp.MustCompile(`^…\S.* #[0-9a-f]{8}$`)
	for _, k := range []string{a, b} {
		if !tag.MatchString(k) || !strings.HasSuffix(k[:len(k)-10], "/d/AGENTS.md") || utf8.RuneCountInString(k) > keyChars {
			t.Fatalf("a long path's key %q (%d characters)", k, utf8.RuneCountInString(k))
		}
	}
	if a == b {
		t.Fatalf("two long paths that end alike share a key: %q", a)
	}
	if c := key("~/x/./" + deep); c != a {
		t.Fatalf("one long path spelled two ways: %q, %q", a, c)
	}
	odd := key("/w/a\nb.md")
	if odd == key("/w/a b.md") || !regexp.MustCompile(`^/w/a b\.md #[0-9a-f]{8}$`).MatchString(odd) {
		t.Fatalf("a path with a line break: %q", odd)
	}
	if tab := key("/w/a\tb.md"); tab == odd {
		t.Fatalf("a tab and a line break share a key: %q", tab)
	}
	if wide := "/w/" + strings.Repeat("é", keyChars-3); key(wide) != wide {
		t.Fatalf("a path of %d characters in %d bytes is kept whole: %q", keyChars, len(wide), key(wide))
	}
	if short := key("/w/" + strings.Repeat("e", keyChars-3)); utf8.RuneCountInString(short) != keyChars || strings.Contains(short, "#") {
		t.Fatalf("a path of %d characters is kept whole: %q", keyChars, short)
	}
	if over := key("/w/" + strings.Repeat("e", keyChars-2)); !tag.MatchString(over) || utf8.RuneCountInString(over) > keyChars {
		t.Fatalf("a path of %d characters is hashed: %q", keyChars+1, over)
	}
}

// Reads that found no file recur only by their path: misses of two paths are two signatures, neither a finding
// though together they would reach the bar, while one path missed again and again, in two sessions and however its
// call spells it, is one case, titled by the path that keeps missing.
func TestANotFoundReadRecursOnlyByItsPath(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	ctx := context.Background()
	f := claudeFiles
	st := testStore(t)
	put(t, st, slices.Concat(f.readMissing("s1", "/w/a.md", t0), f.readMissing("s1", "/w/a.md", t0.Add(time.Hour)), f.readMissing("s2", "/w/b.md", t0.Add(2*time.Hour)))...)
	if fs, err := (Failures{}).Detect(ctx, st, week()); err != nil || len(fs) != 0 {
		t.Fatalf("two paths: %v, %v", signatures(fs), err)
	}
	fs, err := Failures{Recur: Recur{Events: 1}}.Detect(ctx, st, week())
	if got, want := signatures(fs), []string{"failures/v2:miss:read:/w/a.md 2", "failures/v2:miss:read:/w/b.md 1"}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("two paths at a bar of one: %v, %v, want %v", got, err, want)
	}

	st = testStore(t)
	put(t, st, slices.Concat(f.readMissing("s1", "/home/u/p/AGENTS.md", t0), f.readMissing("s1", "/home/u/p/./AGENTS.md", t0.Add(time.Hour)),
		f.readMissing("s2", "~/p/AGENTS.md", t0.Add(2*time.Hour)), f.readMissing("s3", "/home/u/q/AGENTS.md", t0))...)
	outs, _, err := Run(ctx, st, []Detector{Failures{}}, week(), "test", false)
	if err != nil || len(outs) != 1 || len(outs[0].Filed) != 1 || outs[0].Filed[0].Observed != store.Opened {
		t.Fatalf("one path: %+v, %v", outs, err)
	}
	c := outs[0].Filed[0].Case
	if !slices.Equal(c.Signatures, []string{"failures/v2:miss:read:~/p/AGENTS.md"}) || c.Evidence != 3 || c.Title != "The same path keeps missing through read: ~/p/AGENTS.md" {
		t.Fatalf("case %+v", c)
	}
	if cs, err := st.Cases(ctx, store.CaseFilter{All: true}); err != nil || len(cs) != 1 {
		t.Fatalf("cases %d, %v", len(cs), err)
	}
}

// Hermes's searches of a path that is not there recur only by their path, as reads do: two directories are two
// signatures, one directory searched twice is one, and no key of their words merges them.
func TestASearchOfAMissingPathRecursOnlyByItsPath(t *testing.T) {
	f := fileTools{harness: "hermes"}
	search := func(session, path, text string, at time.Time) []store.Event {
		return f.pair(session, "search_files", fmt.Sprintf(`{"pattern":"x","path":%q}`, path), text, true, at)
	}
	st := testStore(t)
	put(t, st, slices.Concat(search("s1", "/w/a", hermesNoPath("/w/a"), t0), search("s2", "/w/a", hermesNoPath("/w/a. Similar paths: /w/ab"), t0.Add(time.Hour)),
		search("s3", "/w/b", hermesNoPath("/w/b"), t0.Add(2*time.Hour)))...)
	fs, err := Failures{Recur: Recur{Events: 1}}.Detect(context.Background(), st, week())
	if got, want := signatures(fs), []string{"failures/v2:miss:search-files:/w/a 2", "failures/v2:miss:search-files:/w/b 1"}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("got %v, %v, want %v", got, err, want)
	}
}

// A read is cleared by its thread's write of the path its call spells, as the store keeps both calls, not by the path
// its key holds: a read of a file under the home, then the write of it, is no failure.
func TestAMissUnderTheHomeClearsByItsCallsPath(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	const x = "/home/u/out/x.md"
	st := testStore(t)
	f := claudeFiles
	put(t, st, slices.Concat(f.readMissing("s1", x, t0), f.wrote("s1", x, t0.Add(time.Minute)),
		f.readMissing("s1", x, t0.Add(time.Hour)), f.wrote("s1", x, t0.Add(time.Hour+time.Minute)),
		f.readMissing("s2", x, t0), f.wrote("s2", x, t0.Add(time.Minute)))...)
	if fs, err := (Failures{}).Detect(context.Background(), st, week()); err != nil || len(fs) != 0 {
		t.Fatalf("found %v, %v", signatures(fs), err)
	}
}

// A relative read is cleared by its thread's write of that path in the directory its call was made in, when that is a
// whole path, as by a write of the path as its call spells it: by a file tool's whole path or by the shell's. Not for
// Hermes, whose read_file resolves it in its terminal's directory, nor for a path under the home or a whole one, nor in
// a directory that is no whole path.
func TestARelativeReadClearsByItsWriteInItsCallsDirectory(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	for _, tc := range []struct {
		name, harness, read, cwd, readIn, missing, write, writeIn, wrote string
		cleared                                                          bool
	}{
		{"kimi", "kimi", "Read", "/w", `{"path":"out/x.md"}`, `"out/x.md" does not exist.`, "Write", `{"path":"/w/out/x.md","content":"x"}`, "Wrote 1 bytes to /w/out/x.md", true},
		{"opencode", "opencode", "read", "/w", `{"filePath":"./out/x.md"}`, "File not found: /w/out/x.md", "write", `{"filePath":"/w/out/x.md","content":"x"}`, "Wrote file successfully.", true},
		{"the shell", "kimi", "Read", "/w", `{"path":"out/x.md"}`, `"out/x.md" does not exist.`, "Bash", `{"command":"cat > out/x.md <<'EOF'\nx\nEOF"}`, "", true},
		{"hermes", "hermes", "read_file", "/w", `{"path":"out/x.md"}`, `{"error": "File not found: /w/out/x.md"}`, "write_file", `{"path":"/w/out/x.md","content":"x"}`, `{"bytes_written": 1}`, false},
		{"a directory that is no whole path", "kimi", "Read", "w", `{"path":"out/x.md"}`, `"out/x.md" does not exist.`, "Write", `{"path":"w/out/x.md","content":"x"}`, "Wrote 1 bytes to w/out/x.md", false},
		{"a path under the home", "kimi", "Read", "/w", `{"path":"~/out/x.md"}`, `"~/out/x.md" does not exist.`, "Write", `{"path":"/w/~/out/x.md","content":"x"}`, "Wrote 1 bytes to /w/~/out/x.md", false},
		{"a whole path", "kimi", "Read", "/w", `{"path":"/v/out/x.md"}`, `"/v/out/x.md" does not exist.`, "Write", `{"path":"/w/v/out/x.md","content":"x"}`, "Wrote 1 bytes to /w/v/out/x.md", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testStore(t)
			f := fileTools{harness: tc.harness}
			var evs []store.Event
			for i, session := range []string{"s1", "s1", "s2"} {
				at := t0.Add(time.Duration(i) * time.Hour)
				pair := slices.Concat(f.pair(session, tc.read, tc.readIn, tc.missing, true, at), f.pair(session, tc.write, tc.writeIn, tc.wrote, false, at.Add(time.Minute)))
				for k := range pair {
					pair[k].Cwd = tc.cwd
				}
				evs = append(evs, pair...)
			}
			put(t, st, evs...)
			fs, err := Failures{}.Detect(context.Background(), st, week())
			if err != nil || tc.cleared && len(fs) != 0 || !tc.cleared && (len(fs) != 1 || len(fs[0].Evidence) != 3) {
				t.Fatalf("found %v, %v, cleared %v", signatures(fs), err, tc.cleared)
			}
		})
	}
}

// OpenCode's write and Hermes's write_file put their content before their path, so the store's cut of the call often
// leaves out the path. Such a write names its file by its result where the harness names it there: OpenCode's line (its
// resource, relative to the call's directory, or whole), Hermes's resolved_path. A result that names no path, or one
// the store cut before the path ends, still clears nothing.
func TestACutWriteClearsByThePathItsResultNames(t *testing.T) {
	const x = "/w/out/x.md"
	cut := `{"content":"` + strings.Repeat("x", 240) + `…`
	for _, tc := range []struct {
		name, harness, cwd, call, wrote string
		cleared                         bool
	}{
		{"opencode whole", "opencode", "/v", cut, "Created file successfully: " + x, true},
		{"opencode relative", "opencode", "/w", cut, "Wrote file successfully: out/x.md", true},
		{"opencode in its project", "opencode", "/w/sub", cut, "Created file successfully: ../out/x.md", true},
		{"opencode cut inside its path", "opencode", "/w", `{"content":"x","path":"/w/ou…`, "Created file successfully: out/x.md", true},
		{"hermes", "hermes", "/v", cut, `{"bytes_written": 1, "dirs_created": true, "verified": true, "lint": {"status": "ok", "output": ""}, "resolved_path": "` + x + `", "files_modified": ["` + x + `"]}`, true},
		{"hermes result cut after its path", "hermes", "", `{"content":"x","path":"/w/ou…`, `{"bytes_written": 1, "dirs_created": true, "resolved_path": "` + x + `", "files_modi…`, true},
		{"opencode 1.x names no path", "opencode", "/w", cut, "Wrote file successfully.", false},
		{"opencode result cut", "opencode", "/w", cut, "Created file successfully: out/x.md…", false},
		{"opencode relative in no whole directory", "opencode", "w", cut, "Created file successfully: out/x.md", false},
		{"hermes result cut before its path", "hermes", "/w", cut, `{"bytes_written": 1, "dirs_created": true, "lint": {"status": "error", "output": "` + x + `: lin…`, false},
		{"hermes path in a nested field", "hermes", "/w", cut, `{"bytes_written": 1, "lint": {"resolved_path": "` + x + `"}, "hint": "x…`, false},
		{"hermes error", "hermes", "/w", cut, `{"bytes_written": 0, "dirs_created": false, "error": "Failed to write file: x", "resolved_path": "` + x + `"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testStore(t)
			f := fileTools{harness: tc.harness}
			read, missing := "read", "File not found: "+x
			if tc.harness == "hermes" {
				read, missing = "read_file", hermesMissing(x)
			}
			write := map[string]string{"opencode": "write", "hermes": "write_file"}[tc.harness]
			var evs []store.Event
			for i, session := range []string{"s1", "s1", "s2"} {
				at := t0.Add(time.Duration(i) * time.Hour)
				pair := slices.Concat(f.pair(session, read, fmt.Sprintf(`{"path":%q}`, x), missing, true, at),
					f.pair(session, write, tc.call, tc.wrote, false, at.Add(time.Minute)))
				for k := range pair {
					pair[k].Cwd = tc.cwd
				}
				evs = append(evs, pair...)
			}
			put(t, st, evs...)
			fs, err := Failures{}.Detect(context.Background(), st, week())
			if err != nil || tc.cleared && len(fs) != 0 || !tc.cleared && (len(fs) != 1 || len(fs[0].Evidence) != 3) {
				t.Fatalf("found %v, %v, cleared %v", signatures(fs), err, tc.cleared)
			}
		})
	}
}

// Each harness's read: the same path missed three times in two sessions is one finding by that path, and a miss of
// another path beside them is not in it.
func TestEachHarnesssMissIsKeyedByItsPath(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	for _, tc := range []struct{ harness, tool, call, missing, origin string }{
		{"claude", "Read", `{"file_path":"@"}`, claudeMissing, "miss:read"},
		{"kimi", "Read", `{"path":"@"}`, `"@" does not exist.`, "miss:read"},
		{"hermes", "read_file", `{"path":"@","limit":100}`, hermesMissing("@"), "miss:read-file"},
		{"opencode", "read", `{"filePath":"@"}`, "File not found: @", "miss:read"},
		{"pi", "read", `{"path":"@"}`, "ENOENT: no such file or directory, access '@'", "miss:read"},
		{"grok", "read_file", `{"target_file":"@"}`, "Error: @ does not exist.", "miss:read-file"},
		{"codex", "read_file", `{"file_path":"@"}`, codexMissing, "miss:read-file"},
	} {
		t.Run(tc.harness, func(t *testing.T) {
			st := testStore(t)
			f := fileTools{harness: tc.harness}
			miss := func(session, p string, at time.Time) []store.Event {
				return f.pair(session, tc.tool, strings.ReplaceAll(tc.call, "@", p), strings.ReplaceAll(tc.missing, "@", p), true, at)
			}
			put(t, st, slices.Concat(miss("s1", "/home/u/p/AGENTS.md", t0), miss("s1", "/home/u/p/AGENTS.md", t0.Add(time.Hour)),
				miss("s2", "/home/u/p/AGENTS.md", t0.Add(2*time.Hour)), miss("s2", "/home/u/p/README.md", t0.Add(2*time.Hour)))...)
			fs, err := Failures{}.Detect(context.Background(), st, week())
			if got, want := signatures(fs), []string{"failures/v2:" + tc.origin + ":~/p/AGENTS.md 3"}; err != nil || !slices.Equal(got, want) {
				t.Fatalf("found %v, %v, want %v", got, err, want)
			}
		})
	}
}

// Hermes's terminal whose directory is gone fails every read with Python's errno for that directory: the reads of
// different paths are one failure, which recurs.
func TestAGoneTerminalDirectoryRecursAsOneFailure(t *testing.T) {
	st := testStore(t)
	f := fileTools{harness: "hermes"}
	gone := `{"error": "[Errno 2] No such file or directory: '/w/gone'"}`
	put(t, st, slices.Concat(f.pair("s1", "read_file", `{"path":"/w/a.md"}`, gone, true, t0), f.pair("s1", "read_file", `{"path":"/w/b.md"}`, gone, true, t0.Add(time.Minute)),
		f.pair("s2", "read_file", `{"path":"c.md"}`, gone, true, t0.Add(2*time.Minute)))...)
	fs, err := Failures{}.Detect(context.Background(), st, week())
	if got, want := signatures(fs), []string{"failures/v2:tool:read-file:[errno N] no such file or directory: Q 3"}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("found %v, %v, want %v", got, err, want)
	}
}
