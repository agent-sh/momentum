package pi

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/internal/fake"
	"github.com/agent-sh/momentum/sessions/internal/saved"
	"github.com/agent-sh/momentum/sessions/proc"
)

func TestAResumedRunDerivesWhatAColdRunDerives(t *testing.T) {
	agent, cwd := t.TempDir(), t.TempDir()
	boot := time.Unix(fake.Boot, 0).UTC()
	start := boot.Add(10 * time.Second)
	created := start.Add(time.Second + 152*time.Millisecond)
	path := SessionDir(agent, cwd) + "/" + created.Format("2006-01-02T15-04-05") + "-152Z_a.jsonl"
	fake.Lines(t, path, map[string]any{"type": "session", "cwd": cwd}, map[string]any{"type": "model_change", "modelId": "local-model"})
	tb, err := proc.Read(fake.ProcRoot(t, fake.Proc{PID: 1, Argv: []string{"pi"}, Start: 1000, Cwd: cwd}))
	if err != nil {
		t.Fatal(err)
	}
	at := start.Add(time.Minute)
	next := func() time.Time { at = at.Add(time.Second); return at }
	run := func(mem *saved.Mem) []*sessions.Session {
		t.Helper()
		src := New(sessions.Env{PiDir: agent})
		if mem != nil {
			src.Resume(mem)
		}
		ss, err := sessions.Scan(context.Background(), &sessions.World{Now: at.Add(time.Minute), Procs: tb}, src)
		if err != nil || len(ss) != 1 {
			t.Fatalf("got %v %+v", err, ss)
		}
		if mem != nil {
			mem.Save(src.Checkpoints())
		}
		return ss
	}
	mem := saved.New()
	round := func(name string) []*sessions.Session {
		t.Helper()
		resumed := run(mem)
		if diff := saved.Same(resumed, run(nil)); diff != "" {
			t.Fatalf("%s: %s", name, diff)
		}
		return resumed
	}
	tool := func(id, cmd string) map[string]any {
		return map[string]any{"type": "toolCall", "id": id, "name": "bash", "arguments": map[string]any{"command": cmd}}
	}
	result := func(id, out string) map[string]any {
		return map[string]any{"type": "message", "timestamp": next().Format(time.RFC3339Nano),
			"message": map[string]any{"role": "toolResult", "toolCallId": id, "content": []any{txt(out)}}}
	}

	fake.Append(t, path, msg(next(), "user", "", txt("list the files")), msg(next(), "assistant", "toolUse", tool("c1", "ls")))
	if ss := round("a call runs"); ss[0].Phase != sessions.Working || ss[0].Now != "bash: ls" {
		t.Fatalf("got %s %q", ss[0].Phase, ss[0].Now)
	}
	fake.Append(t, path, result("c1", "a b"), msg(next(), "assistant", "stop", txt("Two files.")))
	round("the turn ends")
	fake.Append(t, path, msg(next(), "user", "", txt("again")),
		map[string]any{"type": "message", "message": map[string]any{"role": "assistant", "stopReason": "error", "errorMessage": "rate limited"}})
	if ss := round("an error"); ss[0].AskKind != sessions.AskError {
		t.Fatalf("got %+v", ss[0])
	}
	var bulk []any
	for i := 0; i < sessions.LogKeep/2+100; i++ { // past LogKeep, so the log drops its oldest quarter
		id := fmt.Sprintf("b%d", i)
		bulk = append(bulk, msg(next(), "assistant", "toolUse", tool(id, fmt.Sprintf("cat f%d", i))), result(id, "x"))
	}
	fake.Append(t, path, bulk...)
	fake.Append(t, path, msg(next(), "assistant", "stop", txt("Read them all.")))
	if ss := round("a long log"); ss[0].Log.Base() == 0 {
		t.Fatal("the log did not trim")
	}

	// pi writes the file again from the start, shorter
	fake.Lines(t, path, map[string]any{"type": "session", "cwd": cwd}, msg(next(), "user", "", txt("a new branch")))
	round("rewritten")
	fake.Append(t, path, msg(next(), "assistant", "toolUse", tool("c2", "make")))
	round("after the rewrite")
	if mem.Loads == 0 {
		t.Fatal("no run resumed from a checkpoint")
	}

	// a line already read is edited in place: a cold run sees it, a resumed one does not read it again
	var more []any
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("e%d", i)
		more = append(more, result(id, "filler"), msg(next(), "assistant", "toolUse", tool(id, "true")))
	}
	fake.Append(t, path, more[:8]...)
	fake.Append(t, path, msg(next(), "user", "", txt("the middle line")))
	fake.Append(t, path, more[8:]...)
	round("more")
	body, _ := os.ReadFile(path)
	edited := strings.Replace(string(body), "the middle line", "THE MIDDLE LINE", 1)
	if edited == string(body) || os.WriteFile(path, []byte(edited), 0o600) != nil {
		t.Fatal("setup")
	}
	logText := func(ss []*sessions.Session) string { items, _ := ss[0].Log.Since(0); return fmt.Sprint(items) }
	if resumed, cold := run(mem), run(nil); strings.Contains(logText(resumed), "THE MIDDLE") || !strings.Contains(logText(cold), "THE MIDDLE") {
		t.Fatal("the resumed run read the file again from its start")
	}
}
