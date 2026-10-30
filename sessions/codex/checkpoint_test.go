package codex

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
	home := t.TempDir()
	main := newRollout(t, home, mainID)
	child := newRollout(t, home, childID)
	ms := t0.UnixMilli()
	state(t, home, [][]any{
		{mainID, main.path, ms / 1000, ms / 1000, "cli", "/w/app", "refactor the store", nil, nil, "gpt-test", ms, ms + 5000, "user", nil},
		{childID, child.path, ms / 1000, ms / 1000, `{"subagent":{}}`, "/w/app", "", "Kant", nil, "gpt-test", ms, ms + 9000, "subagent", nil},
	}, [][]any{{mainID, childID, "open"}})
	tb, err := proc.Read(fake.ProcRoot(t, fake.Proc{PID: 5, Argv: []string{"codex"}, Start: 500}))
	if err != nil {
		t.Fatal(err)
	}
	found := listed{{Tool: "codex", Thread: mainID, PID: 5, Transcript: main.path}}
	run := func(mem *saved.Mem, now time.Time) []*sessions.Session {
		t.Helper()
		src := &Source{Env: sessions.Env{CodexHome: home}, Lister: found}
		defer src.Close()
		if mem != nil {
			src.Resume(mem)
		}
		ss, err := sessions.Scan(context.Background(), &sessions.World{Now: now, Procs: tb}, src)
		if err != nil {
			t.Fatal(err)
		}
		if mem != nil {
			mem.Save(src.Checkpoints())
		}
		return ss
	}
	mem := saved.New()
	round := func(name string) []*sessions.Session {
		t.Helper()
		now := main.at.Add(time.Minute)
		resumed := run(mem, now)
		if diff := saved.Same(resumed, run(nil, now)); diff != "" {
			t.Fatalf("%s: %s", name, diff)
		}
		return resumed
	}

	main.start().user("refactor the store").call("c1", "spawn_agent", `{"task_name":"review"}`)
	child.start().call("k1", "exec_command", `{"cmd":"rg TODO"}`)
	if ss := round("a subagent runs"); len(ss[0].Tasks) != 1 || ss[0].Tasks[0].Now != "exec_command: rg TODO" {
		t.Fatalf("subagent: %+v", ss[0].Tasks)
	}
	main.output("c1", "spawned").call("q1", "request_user_input_async", `{"questions":[{"title":"Which branch?"}]}`).output("q1", "asked")
	child.output("k1", "3 matches").done("Found three.")
	round("an async ask while the turn runs")
	main.done("I asked which branch.")
	if ss := round("the turn ends"); ss[0].Phase != sessions.NeedsYou {
		t.Fatalf("an async ask blocks once the turn is over: %s", ss[0].Phase)
	}
	main.user("main").start().call("q2", "request_user_input", `{"questions":[{"question":"Delete the old data?"}]}`)
	round("a blocking ask")
	main.output("q2", "yes")
	for i := 0; i < sessions.LogKeep/2+100; i++ { // past LogKeep, so the log drops its oldest quarter
		id := fmt.Sprintf("b%d", i)
		main.call(id, "exec_command", fmt.Sprintf(`{"cmd":"go test ./pkg%d"}`, i)).output(id, "ok")
	}
	main.done("All packages pass.")
	if ss := round("a long log"); ss[0].Log.Base() == 0 {
		t.Fatal("the log did not trim")
	}

	// the rollout is truncated and written again: shorter than what was read
	if err := os.WriteFile(main.path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	main.start().user("start over")
	round("rewritten")
	filler := func(prefix string) { // enough lines that the edit below is neither in the first bytes nor the last
		for i := 0; i < 10; i++ {
			id := fmt.Sprintf("%s%d", prefix, i)
			main.call(id, "exec_command", `{"cmd":"true"}`).output(id, "ok")
		}
	}
	filler("e")
	main.call("c9", "exec_command", `{"cmd":"echo middle"}`).output("c9", "built")
	filler("f")
	main.done("Built.")
	round("after the rewrite")
	if mem.Loads == 0 {
		t.Fatal("no run resumed from a checkpoint")
	}

	// a line already read is edited in place: a cold run sees it, a resumed one does not read it again
	body, _ := os.ReadFile(main.path)
	edited := strings.Replace(string(body), "echo middle", "ECHO MIDDLE", 1)
	if edited == string(body) || os.WriteFile(main.path, []byte(edited), 0o600) != nil {
		t.Fatal("setup")
	}
	logText := func(ss []*sessions.Session) string {
		items, _ := ss[0].Log.Since(0)
		return fmt.Sprint(items)
	}
	now := main.at.Add(2 * time.Minute)
	if resumed, cold := run(mem, now), run(nil, now); strings.Contains(logText(resumed), "ECHO MIDDLE") || !strings.Contains(logText(cold), "ECHO MIDDLE") {
		t.Fatal("the resumed run read the rollout again from its start")
	}
}
