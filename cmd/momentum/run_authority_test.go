package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/loop"
	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
)

func evidenceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "ordinary.txt"), []byte("password=secret-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("password=secret-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-qm", "synthetic")
	return dir
}

// evidence git joins each option's value to its option and refuses a value that reads as an option: a value is
// never another option of git's, and nothing is written outside the repository, even on a refusal.
func TestAGitOptionsValueIsNoOption(t *testing.T) {
	fakeScrubber(t)
	dir := evidenceRepo(t)
	outside := filepath.Join(t.TempDir(), "outside")
	for sub, spec := range gitSpecs {
		for _, option := range spec.values {
			for _, value := range []string{"--output=" + outside, "-o" + outside} {
				for _, args := range [][]string{{option, value}, {option + "=" + value}} {
					code, _, _ := momentum(append([]string{"evidence", "git", dir, sub}, args...)...)
					if code == 0 {
						t.Errorf("accepted %s %v", sub, args)
					}
					if _, err := os.Stat(outside); !os.IsNotExist(err) {
						t.Fatalf("%s %v wrote outside even on refusal", sub, args)
					}
				}
			}
		}
	}
	code, _, errOut := momentum("evidence", "git", dir, "log", "--pretty", "--output="+outside, "--format=format:REVIEW_MARKER")
	if code == 0 {
		t.Errorf("optional option bypass: %s", errOut)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatal("optional value wrote outside")
	}
	args, err := gitArgs(context.Background(), dir, "log", gitSpecs["log"], []string{"--pretty", "format:%s", "-n", "1"})
	if err != nil || !strings.Contains(strings.Join(args, " "), "--pretty=format:%s -n1") {
		t.Fatalf("ambiguous argv %v: %v", args, err)
	}
}

// What the evidence commands print passes the scrubber first, all of it: git's output and its diagnostics, an
// event's fields and its record. A credential file is in no history query, a blob selector of one is refused, and a
// scrubber that cannot run prints nothing.
func TestEveryEvidenceOutputIsScrubbed(t *testing.T) {
	fakeScrubber(t)
	dir := evidenceRepo(t)
	for _, args := range [][]string{{"show", "HEAD:ordinary.txt"}, {"log", "--format", "secret-value"}, {"grep", "--max-depth", "secret-value", "-e", "x"}} {
		_, out, diagnostic := momentum(append([]string{"evidence", "git", dir}, args...)...)
		if strings.Contains(out+diagnostic, "secret-value") {
			t.Errorf("raw Git output: %v %q %q", args, out, diagnostic)
		}
	}
	// Credential content must be absent even from broad or wildcard history queries.
	os.WriteFile(filepath.Join(dir, ".env"), []byte("credential-only-marker\n"), 0600)
	gitIn(t, dir, "commit", "-qam", "credential fixture")
	for _, args := range [][]string{{"show", "HEAD"}, {"show", "HEAD", "--", ".en*"}, {"grep", "-e", "credential-only-marker"}} {
		_, out, _ := momentum(append([]string{"evidence", "git", dir}, args...)...)
		if strings.Contains(out, "credential-only-marker") {
			t.Errorf("implicit credential content: %v %q", args, out)
		}
	}
	blob := strings.TrimSpace(gitIn(t, dir, "rev-parse", "HEAD:.env"))
	for _, args := range [][]string{{"show", blob}, {"show", ":0:.env"}, {"show", "HEAD:.env"}, {"diff", "HEAD", "--", ".env"}, {"show", "HEAD:.ssh/config"}} {
		if code, out, _ := momentum(append([]string{"evidence", "git", dir}, args...)...); code == 0 || strings.Contains(out, "secret-value") {
			t.Errorf("credential selector %v: %d %q", args, code, out)
		}
	}
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	record := filepath.Join(t.TempDir(), "record.log")
	os.WriteFile(record, []byte("secret-value\n"), 0600)
	e := store.Event{Source: store.Transcripts, SourceKey: "synthetic", TS: time.Now(), Kind: "result", Actor: store.ActorTool, Summary: "secret-value", Ref: record + "@0"}
	if _, err := st.Put(context.Background(), []store.Event{e}); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := momentum("evidence", "show", e.ID()); code != 0 || strings.Contains(out+errOut, "secret-value") {
		t.Errorf("raw event %d %q %s", code, out, errOut)
	}
	loop.Scrubber = []string{filepath.Join(t.TempDir(), "absent")}
	for _, args := range [][]string{{"evidence", "git", dir, "show", "HEAD:ordinary.txt"}, {"evidence", "show", e.ID()}} {
		if code, out, _ := momentum(args...); code == 0 || out != "" {
			t.Errorf("failed scrub emitted bytes: %d %q", code, out)
		}
	}
}

// A record is scrubbed whole before any of it is selected: a tail never starts inside a secret's line, an event's
// long record keeps its end, and a record or a unit's output too long to scrub whole is withheld.
func TestOnlyWholeRecordsAreScrubbedAndShown(t *testing.T) {
	fakeScrubber(t)
	scrub := filepath.Join(t.TempDir(), "scrub")
	os.WriteFile(scrub, []byte("#!/bin/sh\nsed 's/password=Zq7Vx9Lm2Kp4/[masked]/g'\n"), 0700)
	loop.Scrubber = []string{scrub}
	file := filepath.Join(t.TempDir(), "ordinary.log")
	value := "Zq7Vx9Lm2Kp4"
	os.WriteFile(file, []byte("password="+value+"\n"+strings.Repeat("z", readMax-len(value)-1)), 0600)
	if code, out, errOut := momentum("evidence", "file", file, "--tail", "2"); code != 0 || strings.Contains(out, value) || !strings.Contains(out, "[masked]") {
		t.Errorf("partial tail %d %q %s", code, out[:min(len(out), 200)], errOut)
	}
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	os.WriteFile(file, []byte(strings.Repeat("z", 70<<10)+" password="+value+" END\n"), 0600)
	e := store.Event{Source: store.Transcripts, SourceKey: "long", TS: time.Now(), Kind: "result", Actor: store.ActorTool, Ref: file + "@0"}
	st.Put(context.Background(), []store.Event{e})
	if code, out, errOut := momentum("evidence", "show", e.ID()); code != 0 || !strings.Contains(out, "END") || strings.Contains(out, value) {
		t.Errorf("partial event %d len=%d %s", code, len(out), errOut)
	}
	os.WriteFile(file, []byte(strings.Repeat("z", readMax+1)+"password="+value), 0600)
	if code, out, _ := momentum("evidence", "file", file); code != 0 || strings.Contains(out, "zzz") || strings.Contains(out, value) {
		t.Error("oversized record was not withheld")
	}
	// Unit output may be capped only by withholding the whole result.
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\ncat '"+file+"'\n"), 0700)
	os.WriteFile(filepath.Join(bin, "journalctl"), []byte("#!/bin/sh\nexit 0\n"), 0700)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	if code, out, _ := momentum("evidence", "unit", "fixture.service"); code == 0 || out != "" {
		t.Error("oversized unit output was emitted")
	}
}

// A run changes only its own case: it merges it into no other, by a close or a signature, and once another run
// merged it, its id changes nothing of the case it went into.
func TestAMergedCaseGivesARunNoAuthority(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	ctx := context.Background()
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	makeCase := func(sig string) store.Case {
		c, _, err := st.ObserveCase(ctx, store.Finding{Family: "failures", Signature: sig, Title: sig, Evidence: []store.Evidence{{Ref: "file:/fixture/" + sig, At: time.Now()}}}, "test")
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	a, b := makeCase("a"), makeCase("b")
	t.Setenv("MOMENTUM_LOOP_RUN", "synthetic")
	t.Setenv("MOMENTUM_LOOP_CASE", a.ID)
	if code, _, _ := momentum("case", "close", a.ID, "--reason", "duplicate", "--into", b.ID, "--why", "synthetic"); code == 0 {
		t.Error("a run merged another case")
	}
	if code, _, _ := momentum("case", "signature", a.ID, "b"); code == 0 {
		t.Error("signature merged another case")
	}
	// Simulate an authorized merge outside this run. Its old ID must grant no writes.
	if _, err := st.CloseCase(ctx, a.ID, store.Duplicate, b.ID, "synthetic", "test"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"set", a.ID, "--title", "changed through alias"}, {"evidence", a.ID, "file:/fixture/new"}, {"signature", a.ID, "new"}} {
		if code, _, _ := momentum(append([]string{"case"}, args...)...); code == 0 {
			t.Errorf("alias write: %v", args)
		}
	}
	got, _ := st.CaseByID(ctx, b.ID)
	if got.Title != b.Title {
		t.Fatal("destination changed through alias")
	}
}

// A case's lesson is published once: after its receipt, the lesson command writes nothing to tacit.
func TestALessonIsPublishedOnce(t *testing.T) {
	// The normal lesson fixture sets up a synthetic improved case and a fake tacit.
	TestALessonIsTheLoopsObservation(t)
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs, err := st.Cases(context.Background(), store.CaseFilter{All: true})
	if err != nil || len(cs) != 1 {
		t.Fatalf("cases %v %v", cs, err)
	}
	bin := strings.Split(os.Getenv("PATH"), ":")[0]
	marker := filepath.Join(bin, "again")
	os.WriteFile(filepath.Join(bin, "tacit"), []byte(fmt.Sprintf("#!/bin/sh\necho called > '%s'\n", marker)), 0700)
	if code, _, errOut := momentum("loop", "lesson", "--case", cs[0].ID, "--text", "another attempt"); code != 0 {
		t.Fatal(errOut)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("lesson published twice")
	}
}

// A lesson whose write left no receipt is written again with the text it reserved, under its case's name, which
// tacit takes as the same fact; then its receipt ends it.
func TestALessonWhoseWriteLeftNoReceiptIsWrittenTheSame(t *testing.T) {
	TestALessonIsTheLoopsObservation(t)
	ctx := context.Background()
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c, _, err := st.ObserveCase(ctx, store.Finding{Family: "failures", Signature: "uncertain", Title: "fixture", Evidence: []store.Evidence{{Ref: "file:/fixture/uncertain", At: time.Now()}}}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE cases SET status = ?, reason = ? WHERE id = ?`, store.CaseClosed, store.Improved, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER stop_receipt BEFORE INSERT ON case_log WHEN NEW.action = 'published' BEGIN SELECT RAISE(ABORT, 'synthetic interruption'); END`); err != nil {
		t.Fatal(err)
	}
	bin := strings.Split(os.Getenv("PATH"), ":")[0]
	calls := filepath.Join(bin, "calls")
	os.WriteFile(filepath.Join(bin, "tacit"), []byte("#!/bin/sh\nprintf '%s|' \"$@\" >> '"+calls+"'\necho >> '"+calls+"'\n"), 0700)
	if code, _, _ := momentum("loop", "lesson", "--case", c.ID, "--text", "the first lesson"); code == 0 {
		t.Fatal("fixture failed to interrupt receipt")
	}
	if _, err := db.Exec(`DROP TRIGGER stop_receipt`); err != nil {
		t.Fatal(err)
	}
	// tried again with another text, it writes the text it reserved, under the same name: tacit's same fact
	if code, _, errOut := momentum("loop", "lesson", "--case", c.ID, "--text", "another lesson"); code != 0 {
		t.Fatalf("the lesson tried again: %s", errOut)
	}
	b, err := os.ReadFile(calls)
	writes := strings.Split(strings.TrimSpace(string(b)), "\n")
	if err != nil || len(writes) != 2 || writes[0] != writes[1] || !strings.Contains(writes[0], "the first lesson") ||
		!strings.HasSuffix(writes[0], "|momentum-lesson-"+strings.ToLower(c.ID)+"|") {
		t.Fatalf("tacit's writes %q %v", writes, err)
	}
	if code, out, _ := momentum("loop", "lesson", "--case", c.ID, "--text", "a third"); code != 0 || !strings.Contains(out, "published already") {
		t.Fatalf("a published lesson written again: %q", out)
	}
	if b, _ := os.ReadFile(calls); strings.Count(string(b), "\n") != 2 {
		t.Fatalf("tacit called after the receipt: %q", b)
	}
}

// A path selector names a checked blob. A subtree cannot change the root of credential exclusions.
func TestATreeSelectorIsRefused(t *testing.T) {
	fakeScrubber(t)
	dir := evidenceRepo(t)
	if err := os.MkdirAll(filepath.Join(dir, "config"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config", ".env"), []byte("credential-only-marker\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-qm", "fixture")
	for _, selector := range []string{"HEAD:config", "HEAD:", "HEAD:config/"} {
		for _, sub := range [][]string{{"grep", "-e", "credential-only-marker"}, {"show"}} {
			args := append([]string{"evidence", "git", dir}, sub...)
			code, out, diag := momentum(append(args, selector)...)
			if code == 0 || out != "" {
				t.Errorf("%s: %d %q %s", selector, code, out, diag)
			}
		}
	}
	if code, out, diag := momentum("evidence", "git", dir, "show", "HEAD:ordinary.txt"); code != 0 || out == "" {
		t.Fatalf("checked blob: %d %q %s", code, out, diag)
	}
}

// Every command's two output streams are scrubbed whole before a run sees either. A failure emits neither.
func TestARunReadsOnlyScrubbedCommandOutput(t *testing.T) {
	fakeScrubber(t)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	t.Setenv("MOMENTUM_LOOP_RUN", "fixture")
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	e := store.Event{Source: store.Transcripts, SourceKey: "fixture", TS: time.Now(), Actor: store.ActorTool, Kind: "tool_result", Summary: "password=secret-value"}
	if _, err := st.Put(ctx, []store.Event{e}); err != nil {
		t.Fatal(err)
	}
	c, _, err := st.ObserveCase(ctx, store.Finding{Family: "failures", Signature: "fixture", Title: "secret-value", Evidence: []store.Evidence{{Ref: "ev:" + e.ID(), At: e.TS}}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	commands := [][]string{{"evidence", "events", "--json"}, {"case", "show", c.ID, "--json"}, {"case", "list", "--json"}, {"evidence", "events"}, {"secret-value"}}
	for _, args := range commands {
		code, out, diag := momentum(args...)
		if strings.Contains(out+diag, "secret-value") || !strings.Contains(out+diag, "************") {
			t.Errorf("%v: %d %q %q", args, code, out, diag)
		}
		if args[len(args)-1] == "--json" && !json.Valid([]byte(out)) {
			t.Errorf("invalid JSON: %s", out)
		}
	}
	// Both streams must wait even when the second scrub fails after the first succeeded.
	e.SourceKey = "second"
	if _, err := st.Put(ctx, []store.Event{e}); err != nil {
		t.Fatal(err)
	}
	scrub := filepath.Join(t.TempDir(), "scrub")
	text := `#!/bin/sh
data=$(cat)
case "$data" in momentum:*) exit 1;; esac
printf '%s' "$data" | sed 's/secret-value/************/g'
`
	if err := os.WriteFile(scrub, []byte(text), 0700); err != nil {
		t.Fatal(err)
	}
	loop.Scrubber = []string{scrub}
	if code, out, diag := momentum("evidence", "events", "--json", "--limit", "1"); code == 0 || out != "" || diag != scrubFailed {
		t.Fatalf("second scrub failed after stdout escaped: %d %q %q", code, out, diag)
	}

	if err := os.WriteFile(scrub, []byte("#!/bin/sh\ncat >/dev/null\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	loop.Scrubber = []string{scrub}
	for _, args := range commands {
		if code, out, diag := momentum(args...); code == 0 || out != "" || diag != scrubFailed {
			t.Errorf("failed scrub: %v: %d %q %q", args, code, out, diag)
		}
	}
}

// A file's multiline context reaches the scrubber before the read cap or a tail selects its lines.
func TestAFileIsScrubbedBeforeItsLinesAreSelected(t *testing.T) {
	fakeScrubber(t)
	scrub := filepath.Join(t.TempDir(), "scrub")
	// This scrubber masks the body only when both delimiters reach the same invocation.
	script := "#!/bin/sh\nsed ':a;N;$!ba;s/BEGIN\\nblock-value\\nEND/[masked]/g'\n"
	if err := os.WriteFile(scrub, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	loop.Scrubber = []string{scrub}
	file := filepath.Join(t.TempDir(), "ordinary.log")
	for _, text := range []string{"BEGIN\nblock-value\nEND\n", strings.Repeat("safe\n", readMax/5) + "BEGIN\nblock-value\nEND\n"} {
		if err := os.WriteFile(file, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		for _, tail := range []string{"0", "2", "3"} {
			code, out, diag := momentum("evidence", "file", file, "--tail", tail)
			if code != 0 || strings.Contains(out, "block-value") {
				t.Fatalf("tail %s: %d %q %s", tail, code, out, diag)
			}
			if tail != "0" && !strings.Contains(out, "[masked]") {
				t.Errorf("tail was selected before scrub: %q", out)
			}
		}
	}
	// A large file is scrubbed whole before selecting its tail.
	text := strings.Repeat("x", 1<<20) + "partial-line\n" + strings.Repeat("safe\n", (4<<20)/5-10) + "last\n"
	if err := os.WriteFile(file, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, diag := momentum("evidence", "file", file, "--tail", "999999")
	if code != 0 || strings.Contains(out, "partial-line") || !strings.Contains(out, "last") {
		t.Fatalf("window: %d len=%d %s", code, len(out), diag)
	}
}

// The installed text scrubber receives a synthetic PEM-shaped block whole. No key is made or read.
func TestAFileKeepsTheScrubbersMultilineContext(t *testing.T) {
	if _, err := exec.LookPath("scrump"); err != nil {
		t.Skip("text scrubber is not installed")
	}
	was := loop.Scrubber
	t.Cleanup(func() { loop.Scrubber = was })
	loop.Scrubber = []string{"scrump", "--profile", "text", "scrub", "-", "-o", "-"}
	body := strings.Repeat("c3ludGhldGlj", 6)
	text := "-----BEGIN PRIVATE KEY-----\n" + body + "\n-----END PRIVATE KEY-----\n"
	whole, err := loop.Redact(context.Background(), text)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(whole, body) {
		t.Fatal("control did not mask the whole block")
	}
	file := filepath.Join(t.TempDir(), "ordinary.log")
	if err := os.WriteFile(file, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tail := range []string{"0", "2"} {
		code, out, diag := momentum("evidence", "file", file, "--tail", tail)
		if code != 0 || strings.Contains(out, body) {
			t.Fatalf("tail %s: %d %q %s", tail, code, out, diag)
		}
	}
}

// Signature format atoms and display settings cannot run any configured verifier outside a sandbox.
func TestGitEvidenceRunsNoSignatureProgram(t *testing.T) {
	fakeScrubber(t)
	for _, format := range []string{"openpgp", "x509", "ssh"} {
		t.Run(format, func(t *testing.T) {
			dir := evidenceRepo(t)
			marker := filepath.Join(t.TempDir(), "marker")
			verifier := filepath.Join(t.TempDir(), "verify")
			if err := os.WriteFile(verifier, []byte("#!/bin/sh\necho ran > '"+marker+"'\nexit 1\n"), 0700); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"gpg.program", "gpg.openpgp.program", "gpg.x509.program", "gpg.ssh.program"} {
				gitIn(t, dir, "config", key, verifier)
			}
			gitIn(t, dir, "config", "log.showSignature", "true")
			if format == "ssh" {
				signers := filepath.Join(t.TempDir(), "allowed-signers")
				if err := os.WriteFile(signers, []byte("fixture ssh-ed25519 c3ludGhldGlj\n"), 0600); err != nil {
					t.Fatal(err)
				}
				gitIn(t, dir, "config", "gpg.ssh.allowedSignersFile", signers)
			}
			signature := map[string]string{"openpgp": "PGP SIGNATURE", "x509": "SIGNED MESSAGE", "ssh": "SSH SIGNATURE"}[format]
			tree := strings.TrimSpace(gitIn(t, dir, "rev-parse", "HEAD^{tree}"))
			raw := "tree " + tree + "\nauthor Fixture <fixture@example.invalid> 1700000000 +0000\ncommitter Fixture <fixture@example.invalid> 1700000000 +0000\ngpgsig -----BEGIN " + signature + "-----\n c3ludGhldGlj\n -----END " + signature + "-----\n\nfixture\n"
			file := filepath.Join(t.TempDir(), "object")
			if err := os.WriteFile(file, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			sha := strings.TrimSpace(gitIn(t, dir, "hash-object", "-t", "commit", "-w", file))
			code, _, diag := momentum("evidence", "git", dir, "log", "--format=%G?", sha)
			if code == 0 {
				t.Fatalf("repository verifier accepted: %d %s", code, diag)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("verifier ran: %v", err)
			}
		})
	}
}

// scrubFailed is all a run reads of a command whose output the scrubber could not pass: a fixed line, nothing of
// the command's.
const scrubFailed = "momentum: the secret scrubber did not run, so nothing of this command is shown\n"

// A synthetic PEM-shaped block crosses the former window boundary. Its tail keeps the opening context.
func TestAFileScrubsAcrossTheFormerWindowBoundary(t *testing.T) {
	was := loop.Scrubber
	defer func() { loop.Scrubber = was }()
	loop.Scrubber = []string{"scrump", "--profile", "text", "scrub", "-", "-o", "-"}
	body := strings.Repeat("c3ludGhldGlj", 6)
	raw := "-----BEGIN PRIVATE KEY-----\n" + strings.Repeat("YWJj\n", (4<<20)/5+4) + body + "\n-----END PRIVATE KEY-----\n"
	whole, err := loop.Redact(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(whole, body) {
		t.Fatal("control did not mask whole block")
	}
	path := filepath.Join(t.TempDir(), "ordinary.log")
	if err = os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOMENTUM_LOOP_RUN", "fixture")
	code, out, diag := momentum("evidence", "file", path, "--tail", "2")
	t.Logf("code=%d whole masked=%v tail sentinel=%v", code, !strings.Contains(whole, body), strings.Contains(out, body))
	if code != 0 {
		t.Fatalf("file failed %s", diag)
	}
	if strings.Contains(out, body) {
		t.Fatal("window started inside multiline secret")
	}
}

// A file over the whole-text scrub limit is refused before any fragment reaches the scrubber or the run.
func TestAFileTooLargeToScrubWholeIsRefused(t *testing.T) {
	fakeScrubber(t)
	t.Setenv("MOMENTUM_LOOP_RUN", "fixture")
	path := filepath.Join(t.TempDir(), "ordinary.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate((64 << 20) + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	for _, tail := range []string{"0", "2"} {
		code, out, diag := momentum("evidence", "file", path, "--tail="+tail)
		if code == 0 || out != "" || !strings.Contains(diag, "too large to scrub whole") {
			t.Fatalf("oversized file: %d %q %s", code, out, diag)
		}
	}
}

// Flag diagnostics use the supplied stderr, so the run scrubs them too. Neither a successful scrub nor a
// failed one may leak a diagnostic to process stderr.
func TestFlagDiagnosticsStayInTheRunStreams(t *testing.T) {
	for _, fails := range []bool{false, true} {
		t.Run(fmt.Sprint(fails), func(t *testing.T) {
			fakeScrubber(t)
			if fails {
				loop.Scrubber = []string{filepath.Join(t.TempDir(), "absent")}
			}
			t.Setenv("MOMENTUM_LOOP_RUN", "fixture")
			f, err := os.CreateTemp(t.TempDir(), "stderr")
			if err != nil {
				t.Fatal(err)
			}
			was := os.Stderr
			os.Stderr = f
			defer func() { os.Stderr = was; f.Close() }()
			code, out, diag := momentum("evidence", "events", "--limit=secret-value")
			if _, err := f.Seek(0, 0); err != nil {
				t.Fatal(err)
			}
			raw, err := io.ReadAll(f)
			if err != nil {
				t.Fatal(err)
			}
			if code == 0 || len(raw) != 0 || strings.Contains(out+diag, "secret-value") {
				t.Fatalf("diagnostic escaped: %d process=%q out=%q diag=%q", code, raw, out, diag)
			}
			if fails && diag != scrubFailed {
				t.Fatalf("failed scrub: %q", diag)
			}
			if !fails && !strings.Contains(diag, "************") {
				t.Fatalf("no scrubbed diagnostic: %q", diag)
			}
		})
	}
}
