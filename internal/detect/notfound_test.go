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

// A read tool's own error that no file is at its path is keyed by the tool and the path its call names, in each
// harness's words for the miss: the home folded to ~, . and .. cleaned, nothing else taken out. A read whose call names
// no path has nothing to key it by, and what is no read's miss keeps its words.
func TestANotFoundReadIsKeyedByThePathItMissed(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	for _, c := range []struct{ harness, tool, text, call, want string }{
		// each harness's words for a miss, by the path its call names
		{"claude", "Read", claudeMissing, `{"file_path":"/w/a.md"}`, "tool:read:file not found: /w/a.md"},
		{"claude", "Read", "File does not exist. Did you mean b.md?", `{"file_path":"/w/a.md","limit":10}`, "tool:read:file not found: /w/a.md"},
		{"kimi", "Read", `"/w/a.md" does not exist.`, `{"line_offset":1,"path":"/w/a.md"}`, "tool:read:file not found: /w/a.md"},
		{"hermes", "read_file", hermesMissing("/w/a.md"), `{"path":"/w/a.md","limit":200}`, "tool:read-file:file not found: /w/a.md"},
		{"hermes", "read_file", `{"error": "[Errno 2] No such file or directory: '/w/a.md'"}`, `{"path":"/w/a.md"}`, "tool:read-file:file not found: /w/a.md"},
		{"opencode", "read", "File not found: /w/a.md Did you mean one of these? /w/b.md", `{"filePath":"/w/a.md"}`, "tool:read:file not found: /w/a.md"},
		{"pi", "read", "ENOENT: no such file or directory, access '/w/a.md'", `{"path":"/w/a.md"}`, "tool:read:file not found: /w/a.md"},
		{"grok", "read_file", "Error: /w/a.md does not exist. Note: your current working directory is /w", `{"limit":40,"target_file":"/w/a.md"}`, "tool:read-file:file not found: /w/a.md"},
		{"codex", "read_file", "File not found: /w/a.md", `{"path":"/w/a.md"}`, "tool:read-file:file not found: /w/a.md"},
		// the path as the key holds it: the home folded to ~, . and .. cleaned, nothing else taken out
		{"claude", "Read", claudeMissing, `{"file_path":"/home/u/p/AGENTS.md"}`, "tool:read:file not found: ~/p/AGENTS.md"},
		{"hermes", "read_file", hermesMissing("/home/u/p/AGENTS.md"), `{"path":"~/p/AGENTS.md"}`, "tool:read-file:file not found: ~/p/AGENTS.md"},
		{"claude", "Read", claudeMissing, `{"file_path":"/home/u"}`, "tool:read:file not found: ~"},
		{"claude", "Read", claudeMissing, `{"file_path":"/home/uv/p/AGENTS.md"}`, "tool:read:file not found: /home/uv/p/AGENTS.md"},
		{"claude", "Read", claudeMissing, `{"file_path":"/home/u/p/./docs/../AGENTS.md"}`, "tool:read:file not found: ~/p/AGENTS.md"},
		{"claude", "Read", claudeMissing, `{"file_path":"/home/u/../u/p/AGENTS.md"}`, "tool:read:file not found: ~/p/AGENTS.md"},
		{"kimi", "Read", `"./a/../CLAUDE.md" does not exist.`, `{"path":"./a/../CLAUDE.md"}`, "tool:read:file not found: CLAUDE.md"},
		{"claude", "Read", claudeMissing, `{"file_path":"/w//Run-2 Notes.MD/"}`, "tool:read:file not found: /w/Run-2 Notes.MD"},
		{"claude", "Read", claudeMissing, `{"file_path":"/w/a1b2c3d4e5f6/12345.json"}`, "tool:read:file not found: /w/a1b2c3d4e5f6/12345.json"},
		// no path: nothing to key it by
		{"claude", "Read", claudeMissing, "", ""},
		{"claude", "Read", claudeMissing, `{"file_path":"/w/` + strings.Repeat("x", 240) + `…`, ""},
		{"claude", "Read", claudeMissing, `["file_path","/w/a.md"]`, ""},
		{"claude", "Read", claudeMissing, `{"limit":10}`, ""},
		{"claude", "Read", claudeMissing, `{"file_path":""}`, ""},
		{"grok", "read_file", "Error: /w/a.md does not exist.", `{"file":"/w/a.md"}`, ""},
		// what is no read's miss keeps its words
		{"claude", "Read", "<tool_use_error>EACCES: permission denied, open '/w/a.md'</tool_use_error>", `{"file_path":"/w/a.md"}`, "tool:read:eacces: permission denied, open Q"},
		{"claude", "Grep", "<tool_use_error>Path does not exist: /w/a</tool_use_error>", `{"path":"/w/a","pattern":"x"}`, "tool:grep:path does not exist: P"},
		{"claude", "Read", "<tool_use_error>read-guard: path not found in the allowlist</tool_use_error>", `{"file_path":"/w/a.md"}`, "guard:read-guard:path not found in the allowlist"},
		{"hermes", "read_file", "Tool 'read_file' does not exist. Available tools: terminal, todo", `{"path":"/w/a.md"}`, "tool:read-file:tool Q does not exist"},
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
		return strings.TrimPrefix(k.Text, "file not found: ")
	}
	deep := strings.Repeat("d/", 60) + "AGENTS.md"
	a, b := key("/home/u/x/"+deep), key("/home/u/y/"+deep)
	tag := regexp.MustCompile(`^…\S.* #[0-9a-f]{8}$`)
	for _, k := range []string{a, b} {
		if !tag.MatchString(k) || !strings.HasSuffix(strings.TrimRight(k[:len(k)-10], " "), "/d/AGENTS.md") || utf8.RuneCountInString(k) > keyChars {
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
	if short := key("/w/" + strings.Repeat("e", keyChars-3)); utf8.RuneCountInString(short) != keyChars || strings.Contains(short, "#") {
		t.Fatalf("a path of %d characters is kept whole: %q", keyChars, short)
	}
}

// Reads that found no file recur only by their path: misses of two paths are two signatures, neither a finding
// though together they would reach the bar, while one path missed again and again, in two sessions and however its
// call spells it, is one case.
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
	if got, want := signatures(fs), []string{"failures/v2:tool:read:file not found: /w/a.md 2", "failures/v2:tool:read:file not found: /w/b.md 1"}; err != nil || !slices.Equal(got, want) {
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
	if !slices.Equal(c.Signatures, []string{"failures/v2:tool:read:file not found: ~/p/AGENTS.md"}) || c.Evidence != 3 || c.Title != "The read tool keeps failing: file not found: ~/p/AGENTS.md" {
		t.Fatalf("case %+v", c)
	}
	if cs, err := st.Cases(ctx, store.CaseFilter{All: true}); err != nil || len(cs) != 1 {
		t.Fatalf("cases %d, %v", len(cs), err)
	}
}

// Each harness's read: the same path missed three times in two sessions is one finding by that path, and a miss of
// another path beside them is not in it.
func TestEachHarnesssMissIsKeyedByItsPath(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	for _, tc := range []struct{ harness, tool, call, missing, origin string }{
		{"claude", "Read", `{"file_path":"@"}`, claudeMissing, "tool:read"},
		{"kimi", "Read", `{"path":"@"}`, `"@" does not exist.`, "tool:read"},
		{"hermes", "read_file", `{"path":"@","limit":100}`, hermesMissing("@"), "tool:read-file"},
		{"opencode", "read", `{"filePath":"@"}`, "File not found: @", "tool:read"},
		{"pi", "read", `{"path":"@"}`, "ENOENT: no such file or directory, access '@'", "tool:read"},
		{"grok", "read_file", `{"target_file":"@"}`, "Error: @ does not exist.", "tool:read-file"},
		{"codex", "read_file", `{"path":"@"}`, "File not found: @", "tool:read-file"},
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
			if got, want := signatures(fs), []string{"failures/v2:" + tc.origin + ":file not found: ~/p/AGENTS.md 3"}; err != nil || !slices.Equal(got, want) {
				t.Fatalf("found %v, %v, want %v", got, err, want)
			}
		})
	}
}
