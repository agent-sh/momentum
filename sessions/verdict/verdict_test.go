package verdict_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/claude"
	"github.com/agent-sh/momentum/sessions/codex"
	"github.com/agent-sh/momentum/sessions/internal/fake"
	"github.com/agent-sh/momentum/sessions/verdict"
)

func TestIdleReviewMustMatchLatestMessage(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name, last, evidence string
		seq                  int
		want                 sessions.Phase
	}{
		{"invented approval", "Made the jelly 20% bigger. Applied live and visually checked; tests pass.", "Does the 20% larger jelly look right?", 7, sessions.Idle},
		{"old question", "The fix is installed. Tests pass.", "Can you approve the restart?", 7, sessions.Idle},
		{"new activity", "Can you approve the restart?", "Can you approve the restart?", 6, sessions.Idle},
		{"missing evidence", "The update is installed.", "", 7, sessions.Idle},
		{"real question", "Can you approve the restart?", "Can you approve the restart?", 7, sessions.NeedsYou},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &sessions.Session{Phase: sessions.Idle, Since: now.Add(-time.Minute), Seq: 7, LastText: tc.last}
			r := &verdict.Review{Phase: sessions.Idle, At: now, Seq: tc.seq, Verdict: verdict.VerdictNeedsYou, NeedsYou: "Approve the restart"}
			fields, _ := json.Marshal(map[string]string{"ask_evidence": tc.evidence, "reviewed_text": tc.last})
			json.Unmarshal(fields, r)
			verdict.Apply(s, r, false)
			if s.Phase != tc.want {
				t.Fatalf("got %s, want %s", s.Phase, tc.want)
			}
		})
	}
}

func TestReviewCannotOverrideExactActivity(t *testing.T) {
	now := time.Now()
	for _, phase := range []sessions.Phase{sessions.Working, sessions.Background, sessions.Scheduled, sessions.NeedsYou} {
		s := &sessions.Session{Phase: phase, Since: now.Add(-time.Minute), Seq: 1, LastText: "Approve the restart?", AskKind: sessions.AskPermission}
		r := &verdict.Review{Phase: sessions.Idle, At: now, Seq: 1, ReviewedText: s.LastText, Verdict: verdict.VerdictDone}
		verdict.Apply(s, r, false)
		if s.Phase != phase || s.AskKind != sessions.AskPermission {
			t.Fatalf("exact activity changed: %#v", s)
		}
	}
}

func TestIdleReviewCompletionAndDismissal(t *testing.T) {
	now := time.Now()
	for _, dismissed := range []bool{false, true} {
		s := &sessions.Session{Key: "s", Phase: sessions.Idle, Since: now.Add(-time.Minute), Seq: 1, LastText: "Approve the restart?"}
		r := &verdict.Review{Phase: sessions.Idle, At: now, Seq: 1, ReviewedText: s.LastText, Verdict: verdict.VerdictDone}
		if dismissed {
			r.Verdict, r.NeedsYou, r.AskEvidence = verdict.VerdictNeedsYou, s.LastText, s.LastText
		}
		verdict.Apply(s, r, dismissed)
		if s.Phase != sessions.Done {
			t.Fatalf("got %s", s.Phase)
		}
	}
}

func TestOldReviewWithoutSourceTextIsNotReused(t *testing.T) {
	now := time.Now()
	s := &sessions.Session{Phase: sessions.Idle, Since: now.Add(-time.Minute), Seq: 1, LastText: "Installed."}
	r := &verdict.Review{Phase: sessions.Idle, At: now, Seq: 1, Verdict: verdict.VerdictNeedsYou, NeedsYou: "Please check it"}
	verdict.Apply(s, r, false)
	if s.Phase != sessions.Idle {
		t.Fatalf("unverified cache promoted to %s", s.Phase)
	}
}

func TestReviewOfAnEarlierStretchIsNotApplied(t *testing.T) {
	now := time.Now()
	s := &sessions.Session{Phase: sessions.Idle, Since: now, Seq: 1, LastText: "Done."}
	r := &verdict.Review{Phase: sessions.Idle, At: now.Add(-time.Minute), Seq: 1, ReviewedText: "Done.", Verdict: verdict.VerdictDone}
	verdict.Apply(s, r, false)
	if s.Phase != sessions.Idle {
		t.Fatalf("a review made before the stretch began settled it: %s", s.Phase)
	}
}

func TestLongFinalMessageRetainsClosingQuestion(t *testing.T) {
	now := time.Now()
	last := strings.Repeat("Completed step. ", 250) + "Can you approve the restart?"
	dir := t.TempDir()
	cl := filepath.Join(dir, "claude.jsonl")
	fake.Lines(t, cl, map[string]any{"type": "assistant", "timestamp": now.Format(time.RFC3339Nano),
		"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": last}}}})
	cx := filepath.Join(dir, "codex.jsonl")
	fake.Lines(t, cx, map[string]any{"type": "event_msg", "timestamp": now.Format(time.RFC3339Nano),
		"payload": map[string]any{"type": "task_complete", "last_agent_message": last}})
	for name, facts := range map[string]func(*sessions.Session, time.Time){"claude": claude.NewTracker(cl).Facts, "codex": codex.NewTracker(cx).Facts} {
		t.Run(name, func(t *testing.T) {
			s := &sessions.Session{Phase: sessions.Idle, Since: now.Add(-time.Minute)}
			facts(s, now)
			s.Seq = s.Log.Seq()
			quote := "Can you approve the restart?"
			if !strings.Contains(verdict.Prompt(s, nil, false, "", now), quote) {
				t.Fatal("closing question missing from review prompt")
			}
			r := &verdict.Review{Phase: sessions.Idle, At: now, Seq: s.Seq, ReviewedText: s.LastText, Verdict: verdict.VerdictNeedsYou, NeedsYou: quote, AskEvidence: quote}
			verdict.Apply(s, r, false)
			if s.Phase != sessions.NeedsYou || s.AskKind != sessions.AskModel {
				t.Fatalf("closing request lost: %s", s.Phase)
			}
		})
	}
}

func TestUnexpectedIdleVerdictSettlesWithoutRepeatedReviews(t *testing.T) {
	now := time.Now()
	for _, v := range []string{"working", "unexpected", ""} {
		s := &sessions.Session{Phase: sessions.Idle, Since: now.Add(-time.Minute), Seq: 1, LastText: "All done."}
		r := &verdict.Review{Phase: sessions.Idle, At: now, Seq: 1, ReviewedText: s.LastText, Verdict: v}
		verdict.Apply(s, r, false)
		if s.Phase != sessions.Done {
			t.Fatalf("verdict %q left phase %q due for another review", v, s.Phase)
		}
	}
}

func TestParseKeepsOnlyEvidencedRequests(t *testing.T) {
	now := time.Now()
	s := &sessions.Session{Phase: sessions.Idle, LastText: "Tests pass.\n\nShould I merge   it now?", Size: 99}
	reply := "Here you go:\n" + `{"dossier":"# Merge the fix\n**State:** green — ready","overall":"Fix is **green**","now":"waiting","needs_you":"Approve the merge","ask_evidence":"Should I merge it now?","verdict":"needs_you"}` + "\nthanks"
	r, dossier, err := verdict.Parse(reply, s, now)
	if err != nil {
		t.Fatal(err)
	}
	if r.Verdict != verdict.VerdictNeedsYou || r.NeedsYou != "Approve the merge" || r.Overall != "Fix is green" || r.ReviewedText != s.LastText || r.Size != 99 {
		t.Fatalf("review %+v", r)
	}
	if dossier != "# Merge the fix\n**State:** green , ready" || verdict.Title(dossier) != "Merge the fix" {
		t.Fatalf("dossier %q", dossier)
	}
	invented := `{"dossier":"","overall":"x","now":"y","needs_you":"Does it look right?","ask_evidence":"Does it look right?","verdict":"needs_you"}`
	if r, _, _ := verdict.Parse(invented, s, now); r.Verdict != verdict.VerdictDone || r.NeedsYou != "" || r.AskEvidence != "" {
		t.Fatalf("a quote not in the last message is turned into done: %+v", r)
	}
	if r, _, _ := verdict.Parse(`{"verdict":"maybe","needs_you":null}`, s, now); r.Verdict != verdict.VerdictWorking {
		t.Fatalf("an unknown verdict reads as working: %+v", r)
	}
	if _, _, err := verdict.Parse("no json here", s, now); err == nil {
		t.Fatal("a reply without JSON is an error")
	}
}

func TestPromptLabelsUserRoleTextAsHarnessRecorded(t *testing.T) {
	if strings.Contains(verdict.System, "Owner's rulings") || !strings.Contains(verdict.System, "not proof that the owner wrote them") {
		t.Fatal("the system prompt must not read user-role lines as the owner's own words")
	}
	now := time.Now()
	s := &sessions.Session{Tool: "claude", Name: "n", Phase: sessions.Background, Tasks: []sessions.Task{{Kind: "bash", Label: "build", Started: now.Add(-time.Minute)}}}
	p := verdict.Prompt(s, []sessions.Entry{{Role: sessions.RoleUser, Origin: "human/typed", Text: "ship it"}, {Role: sessions.RoleOut, Text: "ok"}}, true, "# old", now)
	for _, want := range []string{"agent: Claude Code", "background work is still running", `in flight: bash "build" started 1m ago`,
		"USER [human/typed]: ship it", "older activity since the last review was dropped", "# old"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, p)
		}
	}
}

func TestFitDropsToolOutputFirstAndUserTextLast(t *testing.T) {
	line := func(prefix string, n int) string { return prefix + strings.Repeat("x", n) }
	got := verdict.Fit([]string{line("USER [human/typed]: ", 50), line("OUT: ", 50), line("TOOL Bash: ", 50), line("ASSISTANT: ", 50)}, 200)
	if strings.Contains(got, "OUT: ") || !strings.Contains(got, "TOOL Bash") || !strings.Contains(got, "USER") {
		t.Fatalf("tool output goes first:\n%s", got)
	}
	// the task's first instruction is the oldest line; it outlasts the newer agent lines
	items := []string{line("USER [human/typed]: ", 1000), line("ASSISTANT: ", 50), line("ASSISTANT: ", 50), line("ASSISTANT: ", 50)}
	got = verdict.Fit(items, 400)
	lines := strings.Split(got, "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "USER [human/typed]: ") || len([]rune(lines[0])) != 300 || !strings.HasPrefix(lines[1], "ASSISTANT") {
		t.Fatalf("user-role text outlasts the rest, shortened while far over:\n%s", got)
	}
}
