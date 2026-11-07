package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// treeState is every entry under root by its path: its type and mode, inode, links, size, modification and change
// times, and a regular file's content hash. Any write, a chmod, a rename or a new entry changes it.
func treeState(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		var st syscall.Stat_t
		if err := syscall.Lstat(path, &st); err != nil {
			return err
		}
		line := fmt.Sprintf("mode %o ino %d links %d size %d mtime %d.%09d ctime %d.%09d", st.Mode, st.Ino, st.Nlink, st.Size,
			st.Mtim.Sec, st.Mtim.Nsec, st.Ctim.Sec, st.Ctim.Nsec)
		if d.Type().IsRegular() {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			line += " sha256 " + hashOf(b)
		}
		out[path] = line
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// investigatedConfigCase is an open case whose evidence the signal zz-signal-text counts, as testingCase's is, before
// its investigation.
func investigatedConfigCase(t *testing.T, st *store.Store) store.Case {
	t.Helper()
	ctx := context.Background()
	seedHistory(t, st)
	var evs []store.Event
	for i := 0; i < 6; i++ {
		evs = append(evs, store.Event{Source: store.Transcripts, SourceKey: fmt.Sprintf("r%d@%d", i%2, i), TS: time.Now().Add(-time.Duration(i+1) * time.Hour),
			Session: fmt.Sprint("r", i%2), Harness: "claude", Actor: store.ActorTool, Kind: "tool_result", Tool: "Bash",
			Summary: "Exit code 1 zz-signal-text: the rule broke", Meta: `{"is_error":true}`})
	}
	for _, e := range append([]store.Event(nil), evs...) {
		e.SourceKey += "-baseline"
		e.TS = e.TS.Add(-8 * 24 * time.Hour)
		evs = append(evs, e)
	}
	if _, err := st.Put(ctx, evs); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkRead(ctx, store.Transcripts, time.Now().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	f := store.Finding{Family: "failures", Signature: "failures/v1:rules", Title: "the rule breaks"}
	for _, e := range evs {
		f.Evidence = append(f.Evidence, store.Evidence{Ref: "ev:" + e.ID()})
	}
	c, _, err := st.ObserveCase(ctx, f, "test")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A configuration case runs from its investigation to its close without momentum writing anything outside its own
// directories. Along the way the owner edits the file before the proposal (the build refreshes its base) and moves it
// under the proposal before its review (it is withdrawn and the case builds again); the second proposal is reviewed and
// filed, an agent applies its patch with GNU patch, the loop reads nothing of it, and the case's close as already solved,
// with the fix the note names, ends it. The target's directory and file are read-only to the test process during every
// step of momentum's, its close included, and the whole home, every entry's inode, times, mode and content, is the same
// after each step as before it. Only the owner's and the agent's steps change the file.
func TestAConfigurationCaseWritesNothingOutsideTheLoop(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	home := fakeHome(t)
	dir, file := filepath.Join(home, ".config/agents"), filepath.Join(home, ruleTarget)
	clock := time.Now()
	now := func() time.Time { return clock }
	r := localRunner(t, st)
	r.Now = now
	r.Ship = configShip(t, st, home)
	r.Ship.Dir, r.Ship.Now = r.Dir, now
	readOnly := func() {
		t.Helper()
		if err := os.Chmod(file, 0o444); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	writable := func() {
		os.Chmod(dir, 0o755)
		os.Chmod(file, 0o644)
	}
	t.Cleanup(writable) // the temporary home is removed after this
	readOnly()
	want := treeState(t, home)
	untouched := func(step string) {
		t.Helper()
		got := treeState(t, home)
		if maps.Equal(got, want) {
			return
		}
		var diffs []string
		for _, path := range slices.Sorted(maps.Keys(maps.Clone(got))) {
			if got[path] != want[path] {
				diffs = append(diffs, fmt.Sprintf("%s: %q, was %q", path, got[path], want[path]))
			}
		}
		for path := range want {
			if _, ok := got[path]; !ok {
				diffs = append(diffs, path+": gone")
			}
		}
		t.Fatalf("%s wrote outside the loop's directories:\n%s", step, strings.Join(diffs, "\n"))
	}
	// outside is a step of the owner's or the agent's: the only ones that may change the home
	outside := func(f func()) {
		t.Helper()
		writable()
		f()
		readOnly()
		want = treeState(t, home)
	}
	copies := 0
	copyConfig := func() {
		t.Helper()
		copies++
		copied := filepath.Join(r.Dir, "inputs", fmt.Sprintf("config-%d", copies))
		if err := os.MkdirAll(filepath.Dir(copied), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { removeInputTree(copied) }) // read-only, as a pass's inputs are until the pass ends
		m, err := ConfigCopy(ctx, ConfigCopyOptions{Home: home, Destination: copied})
		if err != nil {
			t.Fatal(err)
		}
		untouched("the configuration copy")
		r.pass = &passInputs{}
		r.pass.manifest.Configuration = m
	}
	var caseID string
	builds := 0
	build := func() Package {
		t.Helper()
		builds++
		run := store.LoopRun{ID: fmt.Sprintf("fixture-build-%d", builds), Case: "", CaseEpisode: 0}
		c, err := st.CaseByID(ctx, caseID)
		if err != nil {
			t.Fatal(err)
		}
		run.Case, run.CaseEpisode, run.Dir = c.ID, c.Episode, filepath.Join(r.Dir, "runs", run.ID)
		p, err := r.buildPackage(ctx, c, run)
		if err != nil || p.Kind != KindConfig || p.Target != ruleTarget {
			t.Fatalf("the build's preparation: %+v %v", p, err)
		}
		untouched("the build's preparation")
		return p
	}
	// the build's run: the change on the build tree's file and its test; then the host's check and proposal
	propose := func(p Package) Package {
		t.Helper()
		base := readFile(t, filepath.Join(p.Work, ruleTarget))
		write(t, filepath.Join(p.Work, ruleTarget), strings.Replace(base, "old rule", "new rule", 1))
		write(t, filepath.Join(p.Work, "tests/test_rules.py"), ruleTest)
		p, err := r.Ship.CheckArgs(ctx, p.Case, []string{"python3", "tests/test_rules.py"}, nil, fixturePatch(t, r.Ship, p.Case))
		if err != nil || p.Check.Before.Exit == 0 || p.Check.After.Exit != 0 {
			t.Fatalf("the check: %+v %v", p.Check, err)
		}
		untouched("the check")
		if p, err = proposePatch(t, r.Ship, ctx, p.Case, configProposal); err != nil || p.State != Proposed || p.PR != "" || p.TargetBase != hashOf([]byte(base)) {
			t.Fatalf("the proposal: %+v %v", p, err)
		}
		untouched("the proposal")
		if n, ok := noteByID(t, st, configNoteID(p.Case)); ok && n.Done.IsZero() {
			t.Fatal("a proposal no review read has a standing note")
		}
		return p
	}
	// the loop's clean-context review, which files a clean proposal for an agent
	file_ := func(p Package) Package {
		t.Helper()
		if _, err := r.Ship.Reviewed(ctx, p.Case, Review{Head: p.Head, Text: "Checked every changed file."}); err != nil {
			t.Fatal(err)
		}
		if done := r.reviews(ctx, p.Case); len(done) != 1 || done[0].Step != "file" {
			t.Fatalf("the review step: %+v", done)
		}
		untouched("the review and the filing")
		p, _, _ = r.Ship.Load(p.Case)
		return p
	}

	// the pass's configuration copy, and the investigation that names the file
	copyConfig()
	c := investigatedConfigCase(t, st)
	report := `{"mechanism":"the rules say the old rule","fix":"say the new rule","summary":"marker-c0ffee","config":"/ro/config/` + ruleTarget + `"}`
	if err := r.keepFindings(ctx, store.LoopRun{Case: c.ID, CaseEpisode: c.Episode, ID: "fixture-investigation"}, nativeFinding(Result{Report: json.RawMessage(report)}), true, ""); err != nil {
		t.Fatal(err)
	}
	untouched("the investigation's findings")
	caseID = c.ID
	if got, step, why, err := r.pick(ctx, "failures", c.ID, false); err != nil || got.ID != c.ID || step != StepBuild {
		t.Fatalf("selection: %q %q %q %v", got.ID, step, why, err)
	}
	untouched("selection")
	first := build()

	// the owner edits the file before anything is proposed: the next pass builds on it as it is
	edit1 := strings.Replace(oldRule, "line d", "line d, the owner's first edit", 1)
	outside(func() { write(t, file, edit1) })
	copyConfig()
	refreshed := build()
	if refreshed.TargetBase != hashOf([]byte(edit1)) || refreshed.BaseCommit == first.BaseCommit {
		t.Fatalf("the build after the owner's edit: %+v", refreshed)
	}
	if log, _ := st.CaseLog(ctx, c.ID); !logHas(log, "prepared", "the file as it is now") {
		t.Fatalf("the refreshed base is not in the case's log: %+v", log)
	}
	propose(refreshed)

	// the owner moves the file under the proposal before its review files it: it is withdrawn, and the case builds
	// again on the file as it is
	edit2 := strings.Replace(edit1, "line a", "line a, the owner's second edit", 1)
	outside(func() { write(t, file, edit2) })
	if got, step, _, err := r.pick(ctx, "failures", c.ID, false); err != nil || got.ID != c.ID || step != StepBuild {
		t.Fatalf("the pass after the move: %q %q %v", got.ID, step, err)
	}
	untouched("the pass that withdraws it")
	copyConfig()
	p := file_(propose(build()))
	if p.TargetBase != hashOf([]byte(edit2)) {
		t.Fatalf("the proposal after the withdrawal: %+v", p)
	}
	n, ok := noteByID(t, st, configNoteID(c.ID))
	fix := "config:~/" + ruleTarget + "@" + p.TargetNew
	if !ok || n.Kind != store.NoteProblem || n.Importance != store.ImportanceLow || !strings.Contains(n.Detail, "--fix '"+fix+"'") || !strings.Contains(n.Detail, p.TargetBase) {
		t.Fatalf("the filed proposal's note: %+v", n)
	}
	raw, err := os.ReadFile(r.Ship.proposalFile(p, ".json"))
	if err != nil {
		t.Fatal(err)
	}
	var rec ConfigProposal
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.File != file || rec.Base != p.TargetBase || rec.New != p.TargetNew || rec.Check == nil || rec.Plan == nil || !strings.HasPrefix(rec.Patch, r.Dir) ||
		rec.Fix != fix || rec.Apply != configApplyLine(home, p, rec.Patch) {
		t.Fatalf("the proposal's record: %+v", rec)
	}
	if _, err := r.Ship.Land(ctx, c.ID); err == nil || !strings.Contains(err.Error(), "configuration proposal") {
		t.Fatalf("the loop landed a configuration proposal: %v", err)
	}
	untouched("a landing refused")

	// the agent applies the patch with its own tools, as the note shows it; the loop reads nothing of it and waits
	applied := strings.Replace(edit2, "old rule", "new rule", 1)
	outside(func() { agentRuns(t, rec.Apply, file, applied) })
	if got, _, why, err := r.pick(ctx, "failures", c.ID, false); err != nil || got.ID != "" || !strings.Contains(why, store.ConfigStage) {
		t.Fatalf("the pass after the agent: %q %q %v", got.ID, why, err)
	}
	untouched("the pass after the agent")
	if p, _, _ = r.Ship.Load(c.ID); p.State != Proposed {
		t.Fatalf("the applied proposal: %s", p.State)
	}

	// the case's close, as the note names it, ends the proposal: the loop lands, deploys and measures nothing
	if _, err := st.CloseSolvedBy(ctx, c.ID, fix, "an agent applied the proposal", "agent-x"); err != nil {
		t.Fatal(err)
	}
	untouched("the close")
	r.pick(ctx, "failures", "", false)
	untouched("the pass after the close")
	if p, _, _ = r.Ship.Load(c.ID); p.State != Withdrawn {
		t.Fatalf("the proposal of the closed case: %s", p.State)
	}
	if n, _ := noteByID(t, st, configNoteID(c.ID)); n.Done.IsZero() {
		t.Fatal("the proposal's note stands after the close")
	}
	if ms, err := r.Measure(ctx, Options{}); err != nil || len(ms) != 0 {
		t.Fatalf("the measure step after the close: %+v %v", ms, err)
	}
	untouched("the measure step")
	if got, _ := st.CaseByID(ctx, c.ID); got.Status != store.CaseClosed || got.Reason != store.Solved || !got.Deployed.IsZero() {
		t.Fatalf("the closed case: %+v", got)
	}
}

// agentRuns is an agent's step with its own tools: the apply line the note shows, when GNU patch and sha256sum are
// here, else the content it would leave. Either way the file must end as want.
func agentRuns(t *testing.T, line, file, want string) {
	t.Helper()
	_, patch := exec.LookPath("patch")
	_, sum := exec.LookPath("sha256sum")
	if patch == nil && sum == nil {
		if out, err := exec.Command("sh", "-c", line).CombinedOutput(); err != nil {
			t.Fatalf("the agent's %s: %v: %s", line, err, out)
		}
	} else {
		write(t, file, want)
	}
	if got := readFile(t, file); got != want {
		t.Fatalf("the agent's step left %q", got)
	}
}
