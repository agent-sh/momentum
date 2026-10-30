package source

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/pkg/evidence"
	"github.com/klauspost/compress/zstd"

	"github.com/agent-sh/momentum/internal/store"
)

// The fixtures are synthetic: the fields each harness writes, the values made up.

const claudeSID = "5d1c2b3a-0000-4000-8000-00000000c1a1"

// claudeLine is a line of the test's Claude Code session at minute m.
func claudeLine(m int, fields string) string {
	return fmt.Sprintf(`{%s,"timestamp":"2026-10-20T10:%02d:00.000Z","isSidechain":false,"userType":"external","entrypoint":"cli","cwd":"/home/u/projects/shop","sessionId":"%s","version":"2.1.302"}`, fields, m, claudeSID)
}

const typed = `"promptSource":"typed","turnOrigin":"human","origin":{"kind":"human"}`

// A reply long enough that its summary keeps only its start and its end.
var longReply = "Checking the cache settings first. " + strings.Repeat("The cache key is built from the lockfile and the toolchain. ", 40) + "Should I run the release build now?"

func claudeSession() []string {
	reply, _ := json.Marshal(longReply)
	return []string{
		claudeLine(0, `"type":"user","uuid":"u0","promptId":"p0",`+typed+`,"message":{"role":"user","content":"<system-reminder>\nthe background build finished\n</system-reminder>\nkeep the cache warm before a release"}`),
		claudeLine(1, `"type":"assistant","uuid":"a1","message":{"model":"claude-opus-5-5","role":"assistant","content":[{"type":"text","text":`+string(reply)+`},{"type":"tool_use","id":"toolu_01","name":"Bash","input":{"command":"make cache-check"}}]}`),
		claudeLine(2, `"type":"user","uuid":"u2","message":{"role":"user","content":[{"tool_use_id":"toolu_01","type":"tool_result","content":"make: *** No rule to make target 'cache-check'.","is_error":true}]}`),
		claudeLine(2, `"type":"system","subtype":"turn_duration","uuid":"s3","durationMs":42000`),
		claudeLine(5, `"type":"user","uuid":"u4","promptId":"p4",`+typed+`,"message":{"role":"user","content":"use the warm target"}`),
	}
}

// A subagent of the session: its prompt is the parent agent's, and it calls a tool that does not exist.
func claudeSubagent() []string {
	sub := func(m int, fields string) string {
		return strings.Replace(claudeLine(m, fields), `"sessionId"`, `"agentId":"a1b2","sessionId"`, 1)
	}
	return []string{
		sub(1, `"type":"user","uuid":"s0","message":{"role":"user","content":"Find where the cache key is built."}`),
		sub(1, `"type":"assistant","uuid":"s1","message":{"model":"claude-sonnet-5-5","role":"assistant","content":[{"type":"tool_use","id":"toolu_g1","name":"Grep","input":{"pattern":"cacheKey"}}]}`),
		sub(2, `"type":"user","uuid":"s2","message":{"role":"user","content":[{"tool_use_id":"toolu_g1","type":"tool_result","content":"<tool_use_error>Error: No such tool available: Grep</tool_use_error>","is_error":true}]}`),
	}
}

func codexLine(m int, typ, payload string) string {
	return fmt.Sprintf(`{"timestamp":"2026-10-21T09:%02d:00.000Z","type":%q,"payload":%s}`, m, typ, payload)
}

func codexRollout() []string {
	user, _ := json.Marshal(map[string]any{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "make the cache tests pass"}}})
	args, _ := json.Marshal(`{"cmd":"go test ./cache"}`)
	out, _ := json.Marshal("Process exited with code 1\nOutput:\n--- FAIL: TestKey\n")
	return []string{
		codexLine(0, "session_meta", `{"id":"019a-own","timestamp":"2026-10-21T09:00:00.000Z","cwd":"/home/u/projects/shop","originator":"codex-tui","cli_version":"0.162.1","source":"cli","thread_source":"user","model_provider":"p"}`),
		codexLine(1, "event_msg", `{"type":"task_started","turn_id":"t1"}`),
		codexLine(1, "turn_context", `{"turn_id":"t1","cwd":"/home/u/projects/shop","model":"gpt-6.1-sol"}`),
		codexLine(1, "response_item", string(user)),
		codexLine(2, "response_item", `{"type":"function_call","name":"exec_command","arguments":`+string(args)+`,"call_id":"call_e1"}`),
		codexLine(3, "response_item", `{"type":"function_call_output","call_id":"call_e1","output":`+string(out)+`}`),
		codexLine(4, "response_item", `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"[Recorded tool history]\nexec_command go test ./cache: exit 1"}]}`),
	}
}

// A rollout with no session_meta: no rule says who typed its turns.
func codexUnknown() []string {
	user, _ := json.Marshal(map[string]any{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "rebuild the index"}}})
	return []string{codexLine(1, "response_item", string(user))}
}

// A pi session a person types into, where they run a command from the prompt themselves.
func piSession() []string {
	return []string{
		`{"type":"session","version":3,"id":"01pi","timestamp":"2026-10-22T08:00:00.000Z","cwd":"/home/u/projects/shop"}`,
		`{"type":"message","id":"u1","parentId":null,"timestamp":"2026-10-22T08:00:05.000Z","message":{"role":"user","content":[{"type":"text","text":"check the tree"}]}}`,
		`{"type":"message","id":"b2","parentId":"u1","timestamp":"2026-10-22T08:00:09.000Z","message":{"role":"bashExecution","command":"git status","output":"fatal: not a git repository","exitCode":128}}`,
	}
}

// home is a synthetic home with the session sources the reader finds, and nothing of the machine's: the
// reader of an owner's session reads the prompt history under $HOME.
func testHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	t.Setenv("TACIT_HERMES_DB", "off")
	return home
}

func writeLines(t *testing.T, path string, lines ...string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func claudePath(home string) string {
	return filepath.Join(home, ".claude", "projects", "-home-u-projects-shop", claudeSID+".jsonl")
}

func subagentPath(home string) string {
	return filepath.Join(home, ".claude", "projects", "-home-u-projects-shop", claudeSID, "subagents", "agent-a1b2.jsonl")
}

func codexPath(home string) string {
	return filepath.Join(home, ".codex", "sessions", "2026", "10", "21", "rollout-2026-10-21T09-00-00-019a-own.jsonl")
}

func codexUnknownPath(home string) string {
	return filepath.Join(home, ".codex", "sessions", "2026", "10", "21", "rollout-2026-10-21T11-00-00-019b-bare.jsonl")
}

func piPath(home string) string {
	return filepath.Join(home, ".pi", "agent", "sessions", "--home-u-projects-shop--", "2026-10-22T08-00-00-000Z_01pi.jsonl")
}

// masked stands in for scrump in tests that do not need it: it masks a token only when it sees it whole, as
// a pattern does, and keeps the length.
var maskRx = regexp.MustCompile(`tok_[A-Za-z0-9]{30}`)

func masked(_ context.Context, evs []evidence.Event) ([]evidence.Event, error) {
	out := make([]evidence.Event, len(evs))
	for i, e := range evs {
		e.Text = maskRx.ReplaceAllStringFunc(e.Text, func(m string) string { return strings.Repeat("*", len(m)) })
		e.ToolInput = maskRx.ReplaceAllStringFunc(e.ToolInput, func(m string) string { return strings.Repeat("*", len(m)) })
		e.ToolOutput = maskRx.ReplaceAllStringFunc(e.ToolOutput, func(m string) string { return strings.Repeat("*", len(m)) })
		out[i] = e
	}
	return out, nil
}

func ready() error { return nil }

func transcripts(home string) Transcripts {
	return Transcripts{Homes: []string{home}, redact: masked, ready: ready}
}

// bySummary finds the one stored event of a kind whose summary contains s.
func find(t *testing.T, evs map[string]store.Event, kind, s string) store.Event {
	t.Helper()
	var hit []store.Event
	for _, e := range evs {
		if e.Kind == kind && strings.Contains(e.Summary, s) {
			hit = append(hit, e)
		}
	}
	if len(hit) != 1 {
		t.Fatalf("%d %s events hold %q", len(hit), kind, s)
	}
	return hit[0]
}

func metaOf(t *testing.T, e store.Event) map[string]any {
	t.Helper()
	m := map[string]any{}
	if e.Meta != "" {
		if err := json.Unmarshal([]byte(e.Meta), &m); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

// Each item becomes an event credited by its kind, with the owner's words alone in his events: the reminder
// the harness put in front of his turn is the harness's, the parent's prompt to a subagent is not his, and a
// long reply keeps its start and its end.
func TestTranscriptsMapEachItemAndKeepTheOwnersWordsApart(t *testing.T) {
	home := testHome(t)
	writeLines(t, claudePath(home), claudeSession()...)
	writeLines(t, subagentPath(home), claudeSubagent()...)
	writeLines(t, codexPath(home), codexRollout()...)
	writeLines(t, codexUnknownPath(home), codexUnknown()...)
	writeLines(t, piPath(home), piSession()...)
	st := openStore(t)
	r := ingest(t, transcripts(home), st)
	evs := all(t, st, "transcripts")
	// Read counts the recorded tool history, which is not stored.
	if r.New != len(evs) || r.Read != len(evs)+1 || len(evs) != 16 {
		t.Fatalf("result %+v, %d stored, want 16", r, len(evs))
	}
	for _, e := range evs {
		if strings.Contains(e.Summary, "Recorded tool history") {
			t.Fatalf("a replay of calls already stored was stored: %+v", e)
		}
	}
	if e := find(t, evs, "agent_text", "rebuild the index"); e.Actor != store.ActorUnknown || !strings.Contains(strings.Join(r.Notes, "\n"), "actor unknown): 1") {
		t.Fatalf("a turn no rule credits: %+v, notes %v", e, r.Notes)
	}
	if e := find(t, evs, "tool_call", "git status"); e.Actor != store.ActorOwner || e.Harness != "pi" || metaOf(t, e)["input_kind"] != "shell" {
		t.Fatalf("a command the owner ran himself: %+v", e)
	}

	owner := find(t, evs, "owner_text", "keep the cache warm")
	if owner.Actor != store.ActorOwner || owner.Summary != "keep the cache warm before a release" || owner.Harness != "claude" ||
		owner.Session != claudeSID || owner.Cwd != "/home/u/projects/shop" || metaOf(t, owner)["input_kind"] != "typed" {
		t.Fatalf("the owner's turn: %+v", owner)
	}
	if e := find(t, evs, "harness_text", "background build finished"); e.Actor != store.ActorHarness {
		t.Fatalf("the reminder in his turn: %+v", e)
	}
	if e := find(t, evs, "owner_text", "use the warm target"); metaOf(t, e)["prior"] != "turn_duration" {
		t.Fatalf("the record before his second turn: %+v", e)
	}
	for _, e := range evs {
		if e.Actor == store.ActorOwner && (strings.Contains(e.Summary, "build finished") || strings.Contains(e.Summary, "Find where")) {
			t.Fatalf("another's text in an owner event: %+v", e)
		}
	}

	reply := find(t, evs, "agent_text", "Checking the cache settings")
	if reply.Actor != store.ActorAgent || reply.Model != "claude-opus-5-5" || !strings.HasSuffix(reply.Summary, "release build now?") ||
		!strings.Contains(reply.Summary, "…") || len(reply.Summary) > headChars+tailChars+8 {
		t.Fatalf("a long reply keeps its start and end: %+v", reply)
	}
	call := find(t, evs, "tool_call", "make cache-check")
	res := find(t, evs, "tool_result", "No rule to make target")
	if call.Actor != store.ActorAgent || call.Tool != "Bash" || res.Actor != store.ActorTool || res.Tool != "Bash" ||
		metaOf(t, res)["is_error"] != true || metaOf(t, call)["tool_id"] != "toolu_01" || metaOf(t, res)["tool_id"] != "toolu_01" {
		t.Fatalf("call %+v\nresult %+v", call, res)
	}
	if !strings.HasPrefix(res.Ref, claudePath(home)+"@") || !strings.HasPrefix(res.SourceKey, claudeSID+"@0000") {
		t.Fatalf("a result's place: ref %q key %q", res.Ref, res.SourceKey)
	}

	prompt := find(t, evs, "agent_text", "Find where the cache key")
	grep := find(t, evs, "tool_result", "No such tool available: Grep")
	if prompt.Actor != store.ActorAgent || prompt.Agent != "a1b2" || prompt.Session != claudeSID || grep.Agent != "a1b2" || metaOf(t, grep)["is_error"] != true {
		t.Fatalf("the subagent's prompt %+v\nits failed call %+v", prompt, grep)
	}

	cx := find(t, evs, "tool_result", "FAIL: TestKey")
	if cx.Harness != "codex" || cx.Model != "gpt-6.1-sol" || cx.Tool != "exec_command" || metaOf(t, cx)["is_error"] != true || cx.Session != "019a-own" {
		t.Fatalf("a Codex result: %+v", cx)
	}
	if e := find(t, evs, "owner_text", "make the cache tests pass"); e.Actor != store.ActorOwner || e.Harness != "codex" {
		t.Fatalf("a Codex owner turn: %+v", e)
	}

	cs, err := st.FileCursors(context.Background(), "transcripts")
	if err != nil || len(cs) != 5 {
		t.Fatalf("cursors %v %v", cs, err)
	}
	for p, c := range cs {
		if len(c.State) == 0 || c.Compressed || c.FP == "" || c.Inode == 0 {
			t.Fatalf("%s: a plain transcript keeps its reader's State: %+v", p, c)
		}
	}
}

func appendText(t *testing.T, path string, lines ...string) {
	t.Helper()
	appendFile(t, path, strings.Join(lines, "\n")+"\n")
}

// A grown transcript is read from its cursor, an unchanged one not at all; a file compressed in place keeps
// its cursor and drops its State; a replaced or rewritten one is read again from its start, its events keeping
// their keys.
func TestTranscriptsReadWhatIsNewAndTellAFileFromANewOne(t *testing.T) {
	home := testHome(t)
	cpath := writeLines(t, claudePath(home), claudeSession()...)
	xpath := writeLines(t, codexPath(home), codexRollout()...)
	st := openStore(t)
	src := transcripts(home)
	first := ingest(t, src, st)

	appendText(t, cpath, claudeLine(6, `"type":"assistant","uuid":"a5","message":{"model":"claude-opus-5-5","role":"assistant","content":[{"type":"text","text":"Using the warm target."}]}`))
	if r := ingest(t, src, st); r.Read != 1 || r.New != 1 {
		t.Fatalf("after an append %+v, want the one new item read", r)
	}
	if r := ingest(t, src, st); r.Read != 0 {
		t.Fatalf("unchanged files were read: %+v", r)
	}
	if os.Geteuid() != 0 {
		// An unchanged file is not even opened.
		if err := os.Chmod(xpath, 0); err != nil {
			t.Fatal(err)
		}
		if r := ingest(t, src, st); r.Read != 0 || len(r.Notes) != 0 {
			t.Fatalf("an unchanged file was opened: %+v", r)
		}
		if err := os.Chmod(xpath, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Compressed in place, as Codex rolls an old rollout.
	raw, err := os.ReadFile(xpath)
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := zstd.NewWriter(nil)
	if err := os.WriteFile(xpath+".zst", enc.EncodeAll(raw, nil), 0o600); err != nil {
		t.Fatal(err)
	}
	enc.Close()
	if err := os.Remove(xpath); err != nil {
		t.Fatal(err)
	}
	if r := ingest(t, src, st); r.Read != 0 || len(r.Notes) != 0 {
		t.Fatalf("a rollout compressed in place was read again: %+v", r)
	}
	cs, _ := st.FileCursors(context.Background(), "transcripts")
	if c := cs[xpath]; !c.Compressed || c.State != nil || c.Offset != int64(len(raw)) {
		t.Fatalf("the compressed copy's cursor: %+v", c)
	}

	// Replaced: a new file at the path, its first line another.
	if err := os.Rename(cpath, cpath+".old"); err != nil {
		t.Fatal(err)
	}
	lines := claudeSession()
	lines[0] = strings.Replace(lines[0], "keep the cache warm before a release", "keep the cache cold before a release", 1)
	writeLines(t, cpath, lines...)
	r := ingest(t, src, st)
	if r.Read != 6 || r.New != 0 || !strings.Contains(strings.Join(r.Notes, "\n"), "read again from the start") {
		t.Fatalf("a replaced transcript %+v, want all 6 items read, each at a place already stored", r)
	}
	evs := all(t, st, "transcripts")
	if len(evs) != first.New+1 {
		t.Fatalf("%d events after the rewrite, want %d: the same places keep their keys", len(evs), first.New+1)
	}
	find(t, evs, "owner_text", "keep the cache cold")

	// Rewritten in place (the same inode), longer than before and opening with another line.
	lines[0] = strings.Replace(lines[0], "keep the cache cold", "keep the cache hot!", 1)
	lines = append(lines, claudeLine(7, `"type":"user","uuid":"u7","promptId":"p7",`+typed+`,"message":{"role":"user","content":"and tag it"}`))
	f, err := os.OpenFile(cpath, os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(strings.Join(lines, "\n") + "\n")
	f.Close()
	// Its new last line is where the reply appended before the rename was: the place keeps its key, and
	// the event there is the line there now.
	if r := ingest(t, src, st); r.Read != 7 || r.New != 0 {
		t.Fatalf("a transcript rewritten in place %+v, want all 7 items read, each at a place already stored", r)
	}
	evs = all(t, st, "transcripts")
	find(t, evs, "owner_text", "keep the cache hot!")
	find(t, evs, "owner_text", "and tag it")
	for _, e := range evs {
		if strings.Contains(e.Summary, "Using the warm target") {
			t.Fatalf("the line no longer there is still stored: %+v", e)
		}
	}

	// Cut in place to less than its cursor, its first line the same: read again from its start.
	f, err = os.OpenFile(cpath, os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(strings.Join(lines[:2], "\n") + "\n")
	f.Close()
	if r := ingest(t, src, st); r.Read != 4 || !strings.Contains(strings.Join(r.Notes, "\n"), "read again from the start") {
		t.Fatalf("a transcript cut in place %+v, want its 4 items read from the start", r)
	}

	// Gone: its cursor is dropped, its events stay.
	n := len(all(t, st, "transcripts"))
	if err := os.Remove(cpath); err != nil {
		t.Fatal(err)
	}
	ingest(t, src, st)
	if cs, _ := st.FileCursors(context.Background(), "transcripts"); len(cs) != 1 || cs[cpath].Path != "" {
		t.Fatalf("cursors after the transcript went: %+v", cs)
	}
	if len(all(t, st, "transcripts")) != n {
		t.Fatal("a gone transcript's events were dropped")
	}
}

// No redaction, no reading: nothing is stored and the run fails. A file whose redaction fails stores nothing
// past its cursor and is named; the others are stored, and the next run takes it from its cursor.
func TestTranscriptsStoreNothingTheyCouldNotRedact(t *testing.T) {
	home := testHome(t)
	writeLines(t, claudePath(home), claudeSession()...)
	xpath := writeLines(t, codexPath(home), codexRollout()...)
	st := openStore(t)

	t.Setenv("PATH", t.TempDir()) // no scrump
	if _, err := (Transcripts{Homes: []string{home}}).Ingest(context.Background(), st); err == nil || !strings.Contains(err.Error(), "redaction unavailable") {
		t.Fatalf("without scrump: %v", err)
	}
	if n := len(all(t, st, "transcripts")); n != 0 {
		t.Fatalf("%d events stored without redaction", n)
	}

	failCodex := func(ctx context.Context, evs []evidence.Event) ([]evidence.Event, error) {
		for _, e := range evs {
			if strings.Contains(e.Text, "go test ./cache") {
				return nil, errors.New("scrump: exit status 1")
			}
		}
		return masked(ctx, evs)
	}
	// One worker: both files go to scrump in one call, which fails, and then each on its own.
	src := Transcripts{Homes: []string{home}, Workers: 1, redact: failCodex, ready: ready}
	r, err := src.Ingest(context.Background(), st)
	if err == nil || !strings.Contains(strings.Join(r.Notes, "\n"), xpath) {
		t.Fatalf("a redaction failure must be named: %v %+v", err, r.Notes)
	}
	for _, e := range all(t, st, "transcripts") {
		if e.Harness == "codex" {
			t.Fatalf("an event of the file whose redaction failed was stored: %+v", e)
		}
	}
	if cs, _ := st.FileCursors(context.Background(), "transcripts"); len(cs) != 1 {
		t.Fatalf("cursors %+v, want only the Claude Code transcript's", cs)
	}
	if r := ingest(t, transcripts(home), st); r.New != 3 {
		t.Fatalf("the next run %+v, want the rollout's 3 events", r)
	}
}

// A summary is cut after redaction, from text cut with a spare: a token across the cut is whole when it is
// masked, so no part of it is stored.
func TestTranscriptsCutSummariesAfterRedaction(t *testing.T) {
	home := testHome(t)
	tok := "tok_" + strings.Repeat("Ab3", 10)
	out, _ := json.Marshal(strings.Repeat("x", textChars-6) + " " + tok + " and the rest of the output")
	lines := claudeSession()[:2]
	lines = append(lines, claudeLine(2, `"type":"user","uuid":"u2","message":{"role":"user","content":[{"tool_use_id":"toolu_01","type":"tool_result","content":`+string(out)+`}]}`))
	writeLines(t, claudePath(home), lines...)
	st := openStore(t)
	ingest(t, transcripts(home), st)
	res := find(t, all(t, st, "transcripts"), "tool_result", "xxxx")
	if strings.Contains(res.Summary, "tok_") || strings.Contains(res.Summary, "Ab3") || !strings.HasSuffix(res.Summary, "*****…") {
		t.Fatalf("a token across the cut: %q", res.Summary)
	}
}

// With scrump, a secret in a call's input is masked before it is stored.
func TestTranscriptsRedactWithScrump(t *testing.T) {
	if err := evidence.RedactionReady(); err != nil {
		t.Skip("needs scrump with its text profile:", err)
	}
	home := testHome(t)
	secret := "pass" + "word=" + "Zq7Vx9" + "Lm2Kp4"
	input, _ := json.Marshal(map[string]string{"command": "curl -u admin " + secret + " https://example.invalid"})
	lines := []string{
		claudeSession()[0],
		claudeLine(1, `"type":"assistant","uuid":"a1","message":{"model":"claude-opus-5-5","role":"assistant","content":[{"type":"tool_use","id":"toolu_01","name":"Bash","input":`+string(input)+`}]}`),
	}
	writeLines(t, claudePath(home), lines...)
	st := openStore(t)
	ingest(t, Transcripts{Homes: []string{home}}, st)
	call := find(t, all(t, st, "transcripts"), "tool_call", "curl -u admin")
	if strings.Contains(call.Summary, "Zq7Vx9") || !strings.Contains(call.Summary, "****") {
		t.Fatalf("the secret was stored: %q", call.Summary)
	}
}

// A batch's texts reach scrump as one text, in one call, each masked as it would be alone. A cut that leaves
// a quoted value open (scrump masks it up to the next quote) or a rule that reaches across lines does not run
// into the next text and hide a secret there: past a quote that closes it, past the start of a text that
// opens with a newline, past a hit the rule's filter rejects (a setting, a reference to a vault). The same
// holds between an agent text's head and tail. As separate records the batch was split and masked again, half
// by half (9 calls here).
func TestTranscriptsRedactABatchInOneCallEachTextAsAlone(t *testing.T) {
	if err := evidence.RedactionReady(); err != nil {
		t.Skip("needs scrump with its text profile:", err)
	}
	real, err := exec.LookPath("scrump")
	if err != nil {
		t.Skip(err)
	}
	shim := t.TempDir()
	calls := filepath.Join(shim, "calls")
	script := "#!/bin/sh\necho call >> '" + calls + "'\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shim, "scrump"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	home := testHome(t)
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	pw := "pass" + "word"
	secrets := []string{"Zq7Vx9" + "Lm2Kp4", "Wq8Ux0" + "Kn3Jp5", "Yr6Tw1" + "Mm4Hq7", "Xs5Sv2" + "Ll5Gr8", "Xt4Rw3" + "Nn6Fs9"}
	// A call's input cut inside an open quoted value, then a result after it.
	pair := func(m int, id, open, result string) []string {
		input, _ := json.Marshal(map[string]string{"command": "deploy " + strings.Repeat("x", textChars+spare-40) + open})
		out, _ := json.Marshal(result)
		return []string{
			claudeLine(m, `"type":"assistant","uuid":"a`+id+`","message":{"model":"claude-opus-5-5","role":"assistant","content":[{"type":"tool_use","id":"toolu_`+id+`","name":"Bash","input":`+string(input)+`}]}`),
			claudeLine(m, `"type":"user","uuid":"u`+id+`","message":{"role":"user","content":[{"tool_use_id":"toolu_`+id+`","type":"tool_result","content":`+string(out)+`}]}`),
		}
	}
	token := " clientToken: 'pub_live_" + strings.Repeat("a1b2", 20) + "'"
	lines := []string{claudeSession()[0]}
	lines = append(lines, pair(1, "1", token, pw+": '"+secrets[0]+"' and the rest")...)
	lines = append(lines, pair(2, "2", token, "\n"+pw+": '"+secrets[1]+"' and the rest")...)
	lines = append(lines, pair(3, "3", " token_url: 'https://auth.example.invalid/oauth/"+strings.Repeat("p", 80)+"'", pw+": '"+secrets[2]+"' and the rest")...)
	lines = append(lines, pair(4, "4", " DB_PASSWORD='$(vault read -field=value secret/db/"+strings.Repeat("q", 80)+")'", pw+": '"+secrets[4]+"' and the rest")...)
	// An agent text: its head is cut inside a quoted token, its tail opens with a newline and holds a password.
	tail := "\n" + strings.Repeat("z ", (tailChars+spare-60)/2) + pw + ": '" + secrets[3] + "' done."
	reply, _ := json.Marshal("Deploying " + strings.Repeat("y", headChars+spare-40) + token + strings.Repeat(" w", 2*spare) + tail)
	lines = append(lines, claudeLine(5, `"type":"assistant","uuid":"a9","message":{"model":"claude-opus-5-5","role":"assistant","content":[{"type":"text","text":`+string(reply)+`}]}`))
	writeLines(t, claudePath(home), lines...)
	st := openStore(t)
	if r := ingest(t, Transcripts{Homes: []string{home}}, st); r.New != 11 {
		t.Fatalf("stored %+v, want 11", r)
	}
	evs := all(t, st, "transcripts")
	for _, e := range evs {
		for _, sec := range secrets {
			if strings.Contains(e.Summary, sec) || strings.Contains(e.Summary, "a1b2a1b2") {
				t.Fatalf("a secret was stored: %+v", e)
			}
		}
	}
	for _, rest := range []string{"and the rest", "done."} {
		for _, e := range evs {
			if strings.Contains(e.Summary, rest) && !strings.Contains(e.Summary, "****") {
				t.Fatalf("a password is not masked: %q", e.Summary)
			}
		}
	}
	b, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	// One call proves redaction can run, one masks the batch.
	if n := strings.Count(string(b), "call"); n != 2 {
		t.Fatalf("%d scrump calls, want 2", n)
	}
}

// The joined call and the second call each mask what the other may not: a text the joined call masked from
// its first byte is masked again on its own, and the two maskings are merged.
func TestTranscriptsMergeTheTwoMaskings(t *testing.T) {
	// A redactor that, joined, runs a hit from "OPEN" into the next text's start and keeps its token; alone,
	// it masks the token.
	run := regexp.MustCompile(`OPEN[^*]*?#\n[a-z]{3}`)
	split := func(_ context.Context, evs []evidence.Event) ([]evidence.Event, error) {
		out := make([]evidence.Event, len(evs))
		for i, e := range evs {
			t := e.Text
			if strings.Contains(t, boundary) {
				t = run.ReplaceAllStringFunc(t, func(m string) string {
					return strings.Map(func(r rune) rune {
						if r == '\n' {
							return r
						}
						return '*'
					}, m)
				})
			} else {
				t = maskRx.ReplaceAllStringFunc(t, func(m string) string { return strings.Repeat("*", len(m)) })
			}
			out[i] = evidence.Event{Text: t}
		}
		return out, nil
	}
	w := writer{redact: split}
	tok := "tok_" + strings.Repeat("Ab3", 10)
	got, err := w.mask(context.Background(), []string{"a value OPEN here", "abc then " + tok + " end"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got[1], "***") || strings.Contains(got[1], tok) || !strings.Contains(got[0], "****") {
		t.Fatalf("merged maskings: %q", got)
	}
}

// hermesSchema is the part of a Hermes store's schema the reader reads.
const hermesSchema = `create table sessions (id text primary key, source text not null, created_source text, model text, model_config text, parent_session_id text, started_at real not null, ended_at real, end_reason text, cwd text, git_repo_root text, title text);
create table messages (id integer primary key autoincrement, session_id text not null references sessions(id), role text not null, content text, tool_call_id text, tool_calls text, tool_name text, timestamp real not null, finish_reason text, _compressed_summary integer default 0, display_kind text);
create index idx_messages_session on messages(session_id, timestamp);`

// A Hermes session the owner types into from his phone: his rows are his, a tool's error the tool's, and a
// row added later is the only one the next run reads.
func TestTranscriptsReadHermesSessionsByTheirLastRow(t *testing.T) {
	home := testHome(t)
	path := filepath.Join(home, ".hermes", "state.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TACIT_HERMES_DB", path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(hermesSchema + `
	  insert into sessions (id, source, created_source, model, started_at) values ('phone', 'mobile', 'mobile', 'gpt-6.1-sol', 100);
	  insert into messages (session_id, role, content, timestamp) values ('phone', 'user', 'prune the docker images', 101);
	  insert into messages (session_id, role, content, tool_calls, finish_reason, timestamp) values ('phone', 'assistant', 'Pruning.',
	    '[{"id":"call_h1","type":"function","function":{"name":"terminal","arguments":"{}"}}]', 'tool_calls', 102);
	  insert into messages (session_id, role, content, tool_call_id, timestamp) values ('phone', 'tool', '{"output": "permission denied", "exit_code": 1}', 'call_h1', 103);`); err != nil {
		t.Fatal(err)
	}
	st := openStore(t)
	src := transcripts(home)
	if r := ingest(t, src, st); r.New != 4 {
		t.Fatalf("first run %+v, want 4 events", r)
	}
	evs := all(t, st, "transcripts")
	own := find(t, evs, "owner_text", "prune the docker images")
	res := find(t, evs, "tool_result", "permission denied")
	if own.Actor != store.ActorOwner || own.Harness != "hermes" || own.Session != "phone" || res.Tool != "terminal" ||
		metaOf(t, res)["is_error"] != true || !strings.HasSuffix(res.Ref, "state.db@3#0") {
		t.Fatalf("owner %+v\nresult %+v", own, res)
	}
	if r := ingest(t, src, st); r.Read != 0 {
		t.Fatalf("a session with no new row was read: %+v", r)
	}
	// Two homes find the same store (the store's own variable): its sessions are read once.
	if _, err := db.Exec(`insert into messages (session_id, role, content, timestamp) values ('phone', 'user', 'and the logs', 105)`); err != nil {
		t.Fatal(err)
	}
	if r := ingest(t, Transcripts{Homes: []string{home, t.TempDir()}, redact: masked, ready: ready}, st); r.Read != 1 {
		t.Fatalf("a store found through two homes: %+v, want the one new row read once", r)
	}
	if os.Geteuid() != 0 {
		// A store that cannot be opened keeps its sessions' cursors: they are not read again once it can.
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		if r := ingest(t, src, st); len(r.Notes) != 1 || !strings.Contains(r.Notes[0], "hermes store") {
			t.Fatalf("an unreadable store must be named: %+v", r)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
		if r := ingest(t, src, st); r.Read != 0 {
			t.Fatalf("after the store could not be opened, its sessions were read again: %+v", r)
		}
	}
	if _, err := db.Exec(`insert into messages (session_id, role, content, timestamp) values ('phone', 'user', 'use sudo then', 110)`); err != nil {
		t.Fatal(err)
	}
	if r := ingest(t, src, st); r.Read != 1 || r.New != 1 {
		t.Fatalf("after a new row %+v, want only it read", r)
	}
}

// A transcript read in parts, whose last part is not stored, is read on from its cursor by the next run:
// only the part that ends a read marks the file read.
func TestTranscriptsReadOnWhenALaterPartWasNotStored(t *testing.T) {
	oldChunk, oldRedact := chunkBytes, redactBytes
	chunkBytes, redactBytes = 1, 1 // a part a line, each part its own scrump call
	t.Cleanup(func() { chunkBytes, redactBytes = oldChunk, oldRedact })
	home := testHome(t)
	lines := claudeSession()
	lines = append(lines, claudeLine(6, `"type":"assistant","uuid":"a9","message":{"model":"claude-opus-5-5","role":"assistant","content":[{"type":"text","text":"LAST-LINE"}]}`))
	writeLines(t, claudePath(home), lines...)
	st := openStore(t)
	failLast := func(ctx context.Context, evs []evidence.Event) ([]evidence.Event, error) {
		for _, e := range evs {
			if strings.Contains(e.Text, "LAST-LINE") {
				return nil, errors.New("scrump: exit status 1")
			}
		}
		return masked(ctx, evs)
	}
	r, err := (Transcripts{Homes: []string{home}, Workers: 1, redact: failLast, ready: ready}).Ingest(context.Background(), st)
	if err == nil || r.New != 6 {
		t.Fatalf("first run %+v %v, want the 6 items before the last stored and a failure", r, err)
	}
	if r := ingest(t, transcripts(home), st); r.Read != 1 || r.New != 1 {
		t.Fatalf("the next run %+v, want the last item read on from the cursor", r)
	}
}

// A file replaced by one that opens with the same line is still another file (its inode): it is read again
// from its start.
func TestTranscriptsReadAgainAFileReplacedWithTheSameFirstLine(t *testing.T) {
	home := testHome(t)
	path := writeLines(t, claudePath(home), claudeSession()...)
	st := openStore(t)
	src := transcripts(home)
	ingest(t, src, st)
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	lines := claudeSession()
	lines = append(lines, claudeLine(7, `"type":"user","uuid":"u7","promptId":"p7",`+typed+`,"message":{"role":"user","content":"and tag it"}`))
	writeLines(t, path, lines...)
	if r := ingest(t, src, st); r.Read != 7 || !strings.Contains(strings.Join(r.Notes, "\n"), "read again from the start") {
		t.Fatalf("a replaced file with the same first line %+v, want all 7 items read from the start", r)
	}
}

// An agent text's summary is made from its masked ends: a token across the head's cut or the tail's is
// whole when it is masked.
func TestTranscriptsMaskAgentTextAtBothCuts(t *testing.T) {
	home := testHome(t)
	tok := func(c string) string { return "tok_" + strings.Repeat(c, 30) }
	text := strings.Repeat("h", headChars-10) + " " + tok("A") + " " + strings.Repeat("m", 3*spare) + " " + tok("B") + strings.Repeat("t", tailChars-12)
	reply, _ := json.Marshal(text)
	lines := []string{claudeSession()[0], claudeLine(1, `"type":"assistant","uuid":"a1","message":{"model":"claude-opus-5-5","role":"assistant","content":[{"type":"text","text":`+string(reply)+`}]}`)}
	writeLines(t, claudePath(home), lines...)
	st := openStore(t)
	ingest(t, transcripts(home), st)
	e := find(t, all(t, st, "transcripts"), "agent_text", "hhhh")
	if strings.Contains(e.Summary, "tok_") || strings.Contains(e.Summary, "AAAA") || strings.Contains(e.Summary, "BBBB") || !strings.Contains(e.Summary, "*****…") {
		t.Fatalf("a token across a cut of the agent's text: %q", e.Summary)
	}
}

// A Codex subagent's thread is a subagent inside its parent's session, as Claude Code's are.
func TestTranscriptsPutASubagentThreadInItsParentsSession(t *testing.T) {
	home := testHome(t)
	user, _ := json.Marshal(map[string]any{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "review the key change"}}})
	lines := []string{
		codexLine(0, "session_meta", `{"id":"019a-sub","timestamp":"2026-10-21T09:00:00.000Z","cwd":"/home/u/projects/shop","originator":"codex-tui","cli_version":"0.162.1","source":{"subagent":"review"},"thread_source":"subagent","parent_thread_id":"019a-own","model_provider":"p"}`),
		codexLine(1, "response_item", string(user)),
	}
	writeLines(t, filepath.Join(home, ".codex", "sessions", "2026", "10", "21", "rollout-2026-10-21T09-10-00-019a-sub.jsonl"), lines...)
	st := openStore(t)
	ingest(t, transcripts(home), st)
	e := find(t, all(t, st, "transcripts"), "agent_text", "review the key change")
	if e.Session != "019a-own" || e.Agent != "019a-sub" || metaOf(t, e)["parent"] != "019a-own" {
		t.Fatalf("a subagent thread: session %q agent %q meta %s", e.Session, e.Agent, e.Meta)
	}
}

// A folder that cannot be listed keeps the cursors of the files in it: they are not read again once it can.
func TestTranscriptsKeepCursorsOfAFolderThatCannotBeListed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root lists any folder")
	}
	home := testHome(t)
	path := writeLines(t, claudePath(home), claudeSession()...)
	st := openStore(t)
	src := transcripts(home)
	ingest(t, src, st)
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	ingest(t, src, st)
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if r := ingest(t, src, st); r.Read != 0 {
		t.Fatalf("after its folder could not be listed, a transcript was read again: %+v", r)
	}
}
