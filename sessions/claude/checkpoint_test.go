package claude

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/internal/fake"
	"github.com/agent-sh/momentum/sessions/internal/saved"
	"github.com/agent-sh/momentum/sessions/proc"
)

// runOnce is one run of a CLI in its own process: a new Source over the same sessions. With a store it resumes
// from the store's checkpoints and saves its own at the end, as momentum does.
func runOnce(t *testing.T, found listed, mem *saved.Mem, now time.Time) []*sessions.Session {
	t.Helper()
	src := &Source{Lister: found, Screen: func(context.Context, *sessions.World, *sessions.Session) string { return "" }}
	if mem != nil {
		src.Resume(mem)
	}
	ss, err := sessions.Scan(context.Background(), &sessions.World{Now: now, Procs: &proc.Table{ByPID: map[int]*proc.Proc{}}}, src)
	if err != nil {
		t.Fatal(err)
	}
	if mem != nil {
		mem.Save(src.Checkpoints())
	}
	sort.Slice(ss, func(i, j int) bool { return ss[i].Key < ss[j].Key })
	return ss
}

func TestAResumedRunDerivesWhatAColdRunDerives(t *testing.T) {
	a := newScript(t) // a foreground session that runs everything a transcript can hold
	b := newScript(t) // a background job: its tasks outlive a respawn
	dirA := strings.TrimSuffix(a.path, ".jsonl")
	out := filepath.Join(t.TempDir(), "b1x9")
	fake.File(t, out+".output", "building\n")
	fake.Lines(t, filepath.Join(dirA, "subagents", "agent-ag1.jsonl"), map[string]any{"type": "assistant",
		"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "reading the diff"}}}})
	found := listed{
		{Tool: "claude", Session: "a", Transcript: a.path, Started: t0},
		{Tool: "claude", Job: "b", Where: "bg", Transcript: b.path},
	}
	mem := saved.New()
	now := t0
	round := func(name string, step time.Duration) []*sessions.Session {
		t.Helper()
		now = now.Add(step)
		for _, x := range []*script{a, b} { // a run comes after the lines it reads
			if x.at.After(now) {
				now = x.at.Add(step)
			}
		}
		resumed := runOnce(t, found, mem, now)
		if diff := saved.Same(resumed, runOnce(t, found, nil, now)); diff != "" {
			t.Fatalf("%s: %s", name, diff)
		}
		return resumed
	}

	a.user("plan the release: "+strings.Repeat("every step in order, ", 30)). // a first line longer than the fingerprint
											call("tu1", "Bash", map[string]any{"command": "make", "description": "Build", "run_in_background": true}).
											result("tu1", "Command running in background with ID: b1x9. Output is being written to: "+out+".output").
											call("tu2", "Agent", map[string]any{"description": "Review the diff", "subagent_type": "reviewer"}).
											result("tu2", "Async agent launched successfully.\nagentId: ag1 (internal)").
											call("tu3", "Workflow", map[string]any{"description": "fan out"}).
											result("tu3", "Workflow launched in background. Task ID: wf1\nSummary: Ten reviewers over the PR\nmore").
											call("tu4", "Monitor", map[string]any{"command": "tail"}).
											result("tu4", "Monitor started (task mon1, expires in 5m)").
											call("tu5", "ScheduleWakeup", map[string]any{"delaySeconds": 600}).
											result("tu5", "Next wakeup scheduled for 10:10:02 (in 600s)").
											call("tu6", "CronCreate", map[string]any{"cron": "*/5 * * * *"}).
											result("tu6", "Scheduled recurring job c1 (*/5 * * * *). It runs until deleted.").
											say("Everything is started.")
	b.user("nightly run: "+strings.Repeat("check every box, ", 40)).
		call("nb1", "Bash", map[string]any{"command": "nightly"}).
		result("nb1", "Command running in background with ID: nb1").say("Running the nightly checks.")
	if ss := round("first", time.Minute); ss[0].Phase != sessions.Background || len(ss[0].Tasks) != 4 || ss[0].Loop == "" {
		t.Fatalf("setup: %s %d tasks loop %q", ss[0].Phase, len(ss[0].Tasks), ss[0].Loop)
	}

	a.notify("ag1", "completed").say("The review is in.").
		call("tu7", "SendMessage", map[string]any{"to": "ag1", "message": "one more pass"}).result("tu7", "sent").
		call("tu8", "CronDelete", map[string]any{"id": "c1"}).result("tu8", "deleted").
		call("tu9", "AskUserQuestion", map[string]any{"questions": []any{map[string]any{"question": "Ship it now?"}}})
	b.add(map[string]any{"type": "queue-operation", "operation": "enqueue",
		"content": "<task-notification><task-id>nb1</task-id><status>failed</status></task-notification>"})
	if err := os.Remove(out + ".output"); err != nil { // its output file is cleaned up while the command runs
		t.Fatal(err)
	}
	if ss := round("asks", 4*time.Minute); ss[0].Phase != sessions.NeedsYou || ss[0].Ask != "Ship it now?" {
		t.Fatalf("question: %s %q", ss[0].Phase, ss[0].Ask)
	}

	a.result("tu9", "yes").call("tu10", "ExitPlanMode", map[string]any{"plan": "1. tag"}).
		add(map[string]any{"type": "user", "isMeta": true, "origin": map[string]any{"kind": "peer"},
			"message": map[string]any{"content": "Another Claude session sent a message: status?"}}).
		add(map[string]any{"type": "user", "isCompactSummary": true, "message": map[string]any{"content": "Summary so far"}})
	b.user("again").add(map[string]any{"type": "assistant", "isApiErrorMessage": true,
		"message": map[string]any{"model": "<synthetic>", "content": []any{map[string]any{"type": "text", "text": "API Error: 529 overloaded"}}}})
	round("plan, and the monitor expires", 4*time.Minute)

	a.result("tu10", "approved")
	for i := 0; i < sessions.LogKeep/2+100; i++ { // past LogKeep, so the log drops its oldest quarter
		id := fmt.Sprintf("bulk%d", i)
		a.call(id, "Read", map[string]any{"file_path": fmt.Sprintf("/w/f%d.go", i)}).result(id, "package x")
	}
	a.call("tu11", "Bash", map[string]any{"command": "sleep 99"}).
		add(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "[Request interrupted by user for tool use]"}}}}).
		user("check it with a helper").call("tu-fg", "Agent", map[string]any{"description": "Find the bug", "subagent_type": "Explore"})
	fake.File(t, filepath.Join(dirA, "subagents", "agent-fg1.meta.json"), `{"toolUseId":"tu-fg"}`)
	fake.Lines(t, filepath.Join(dirA, "subagents", "agent-fg1.jsonl"), map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_use", "id": "x", "name": "Grep", "input": map[string]any{"pattern": "TODO"}}}}})
	if ss := round("a long log", time.Minute); ss[0].Log.Base() == 0 || ss[0].Phase != sessions.Working {
		t.Fatalf("the log did not trim, or the helper does not run: base %d %s", ss[0].Log.Base(), ss[0].Phase)
	}

	// b is rewritten in place: the same first line, other content, longer than what was read
	head, err := os.ReadFile(b.path)
	if err != nil {
		t.Fatal(err)
	}
	first := head[:strings.IndexByte(string(head), '\n')+1]
	if err := os.WriteFile(b.path, append(append([]byte{}, first...), []byte(strings.Repeat(`{"type":"summary","x":"padding"}`+"\n", 80))...), 0o600); err != nil {
		t.Fatal(err)
	}
	b.say("Started over.")
	round("b rewritten", time.Minute)

	// a is replaced by a copy of itself plus more: a new file at the same path
	body, err := os.ReadFile(a.path)
	if err != nil {
		t.Fatal(err)
	}
	fake.File(t, a.path+".tmp", string(body))
	if err := os.Rename(a.path+".tmp", a.path); err != nil {
		t.Fatal(err)
	}
	a.result("tu-fg", "found it").say("Fixed the bug.")
	round("a replaced", time.Minute)

	// a new process resumes a: tasks of the old one died with it
	found[0].Started = now.Add(time.Minute)
	a.at = found[0].Started
	a.user("carry on").say("Carrying on.")
	if ss := round("a respawned", 2*time.Minute); len(ss[0].Tasks) != 0 {
		t.Fatalf("tasks of the earlier process: %+v", ss[0].Tasks)
	}
	if mem.Loads == 0 {
		t.Fatal("no run resumed from a checkpoint")
	}

	// A line already read is edited in place, keeping the file's first bytes and its last ones: a cold run sees
	// the edit, a resumed one does not read that part again.
	body, err = os.ReadFile(a.path)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(body), "/w/f2000.go", "/w/X2000.go", 1)
	if edited == string(body) || os.WriteFile(a.path, []byte(edited), 0o600) != nil {
		t.Fatal("setup")
	}
	now = now.Add(time.Minute)
	resumed, cold := runOnce(t, found, mem, now), runOnce(t, found, nil, now)
	logText := func(s *sessions.Session) string {
		items, _ := s.Log.Since(0)
		var b strings.Builder
		for _, e := range items {
			b.WriteString(e.Text + "\n")
		}
		return b.String()
	}
	if strings.Contains(logText(resumed[0]), "X2000") || !strings.Contains(logText(cold[0]), "X2000") {
		t.Fatal("the resumed run read the transcript again from its start")
	}
}
