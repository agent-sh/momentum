package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

const (
	ruleTarget = ".config/agents/zz-rules.md"
	oldRule    = "# Rules\n\nmarker-c0ffee old rule\n\n## More\n\nline a\nline b\nline c\nline d\n"
	newRule    = "# Rules\n\nmarker-c0ffee new rule\n\n## More\n\nline a\nline b\nline c\nline d\n"
	// ownerEdit is an edit of the owner's away from the change's lines and their context
	ownerEdit = "line e, the owner's own\n"
	ruleTest  = "import sys\ntext = open('.config/agents/zz-rules.md').read()\nsys.exit(0 if 'marker-c0ffee new rule' in text else 1)\n"
)

// fakeHome is a home under the test's own directory with one rules file of the agent-sync root, mode 0640.
func fakeHome(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	write(t, filepath.Join(home, ruleTarget), oldRule)
	if err := os.Chmod(filepath.Join(home, ruleTarget), 0o640); err != nil {
		t.Fatal(err)
	}
	return home
}

func configShip(t *testing.T, st *store.Store, home string) *Ship {
	t.Helper()
	return &Ship{Store: st, Dir: filepath.Join(t.TempDir(), "loop"), Live: true, Actor: "loop:test", Identity: [2]string{"Owner Name", "owner@example.com"},
		Sandbox: testSandbox(t), Public: func(_ context.Context, s string) (string, error) { return Neutral(s), nil },
		ConfigTargets: ConfigTargets{Enabled: true, Allow: []string{ruleTarget}, Home: home}}
}

// configProposal's signal counts nothing yet: a configuration proposal's plan is information, never counted.
var configProposal = Proposal{Title: "Rules say the new rule", Body: "The old rule caused the failure.", Rollback: "apply the reverse patch",
	Lesson: "Rules say the new rule.", Signal: Signal{Source: store.Transcripts, Kinds: []string{"tool_result"}, Match: []string{"zz-never-seen-yet"}},
	Window: 7 * 24 * time.Hour}

// refusedTarget asserts that a target is refused with why in its cause, by its name and when allow names it exactly.
func refusedTarget(t *testing.T, ct ConfigTargets, name, why string) {
	t.Helper()
	ct.Allow = append(append([]string(nil), ct.Allow...), name)
	_, err := ct.admit(name)
	if err == nil && ct.Home != "" {
		var d *targetDir
		if d, err = ct.open(name); err == nil {
			_, err = d.state()
			d.Close()
		}
	}
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), why) {
		t.Errorf("%s: want a refusal for %q, got %v", name, why, err)
	}
}

// allow names exact files: an empty list admits nothing, and a directory or a pattern admits nothing either.
func TestAnEmptyOrInexactAllowAdmitsNoTarget(t *testing.T) {
	if _, err := (ConfigTargets{}).admit(ruleTarget); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "names no file") {
		t.Fatalf("an empty allow: %v", err)
	}
	for _, entry := range []string{".config/agents", ".config/agents/", "~/.config/agents", ".config/agents/*.md", ".config/agents/zz-r?les.md",
		".config/agents/zz-[r]ules.md", ".config/agents/**", "/ro/config/" + ruleTarget, ".config/agents/zz-other.md"} {
		if _, err := (ConfigTargets{Allow: []string{entry}}).admit(ruleTarget); !errors.Is(err, ErrRefused) {
			t.Errorf("allow %q admitted %s: %v", entry, ruleTarget, err)
		}
	}
	for _, entry := range []string{ruleTarget, "~/" + ruleTarget, " " + ruleTarget + " "} {
		if _, err := (ConfigTargets{Allow: []string{entry}}).admit(ruleTarget); err != nil {
			t.Errorf("allow %q: %v", entry, err)
		}
	}
	// a directory named exactly is no regular file
	home := fakeHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".config/agents/DIR.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	refusedTarget(t, ConfigTargets{Home: home}, ".config/agents/DIR.md", "no regular file")
}

// The loop's own configuration and rules are never targets, whatever allow says: momentum's configuration, and every
// file loop.rules names (and the file one resolves to), a policy every run reads among them.
func TestTheLoopsOwnFilesAreNeverTargets(t *testing.T) {
	home := fakeHome(t)
	write(t, filepath.Join(home, ".config/agents/zz-policy.md"), "marker-c0ffee policy\n")
	write(t, filepath.Join(home, ".config/agents/notes/RULES.md"), "marker-c0ffee shared\n")
	write(t, filepath.Join(home, ".config/agents/zz-notes/zz-guide.md"), "marker-c0ffee procedure\n")
	write(t, filepath.Join(home, ".config/momentum/sources.json"), "{}\n")
	// a rules entry that is a link: its file is the loop's own too
	if err := os.Symlink("zz-notes/zz-guide.md", filepath.Join(home, ".config/agents/LINKED.md")); err != nil {
		t.Fatal(err)
	}
	ct := ConfigTargets{Home: home, Own: []string{"~/.config/agents/notes/RULES.md", filepath.Join(home, ".config/agents/LINKED.md"), "~/.config/agents/zz-policy.md"}}
	for _, name := range []string{".config/agents/zz-policy.md", ".config/agents/notes/RULES.md", ".config/agents/zz-notes/zz-guide.md",
		".config/momentum/sources.json", ".config/agents/notes/rules.md"} {
		refusedTarget(t, ct, name, "the loop's own")
	}
	if _, err := (ConfigTargets{Home: home, Allow: []string{ruleTarget}, Own: ct.Own}).admit(ruleTarget); err != nil {
		t.Fatalf("another file: %v", err)
	}
	// with nothing configured, only momentum's own directories are refused as its own: no path of a machine's layout
	for _, o := range fixedOwn {
		if o != ".config/momentum" && o != ".local/share/momentum" {
			t.Errorf("a fixed own path outside momentum's directories: %s", o)
		}
	}
}

// Credential and auth files are never targets.
func TestCredentialFilesAreNeverTargets(t *testing.T) {
	ct := ConfigTargets{}
	for _, name := range []string{".config/agents/bedrock.env", ".config/agents/.env", ".config/agents/x.env.md", ".codex/auth.json",
		".config/agents/auth.md", ".config/agents/keyring.md", ".config/agents/procedures/token-notes.md", ".config/agents/secrets.md",
		".ssh/config", ".aws/credentials", ".claude.json", ".config/agents/id_rsa.md", ".config/agents/oauth.md"} {
		refusedTarget(t, ct, name, "a credential or auth file")
	}
}

// Nothing under a bin directory is a target.
func TestBinDirectoriesAreNeverTargets(t *testing.T) {
	for _, name := range []string{".local/bin/tool", ".local/bin/notes.md", ".config/agents/bin/x.md", "bin/x.md", ".config/agents/sbin/x.md"} {
		refusedTarget(t, ConfigTargets{}, name, "bin directory")
	}
}

// Databases and their journals are never targets.
func TestDatabasesAreNeverTargets(t *testing.T) {
	for _, name := range []string{".config/agents/notes.db", ".config/agents/x.sqlite", ".config/agents/x.sqlite3", ".config/agents/x.md-wal",
		".config/agents/x.md-journal", ".config/agents/store.duckdb"} {
		refusedTarget(t, ConfigTargets{}, name, "a database")
	}
}

// Links, in the file or any directory of its path, and paths that resolve outside the home are never targets.
func TestLinksAndPathsOutsideHomeAreNeverTargets(t *testing.T) {
	home := fakeHome(t)
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	write(t, filepath.Join(elsewhere, "x.md"), "marker-c0ffee elsewhere\n")
	if err := os.Symlink(filepath.Join(elsewhere, "x.md"), filepath.Join(home, ".config/agents/LINK.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(home, ".config/agents/procedures")); err != nil {
		t.Fatal(err)
	}
	ct := ConfigTargets{Home: home}
	refusedTarget(t, ct, ".config/agents/LINK.md", "a link")
	refusedTarget(t, ct, ".config/agents/procedures/x.md", "a link")
	for _, name := range []string{"/etc/passwd", "../x.md", ".config/agents/../../x.md", "~/../x.md", ".config/agents/./zz-rules.md", ""} {
		refusedTarget(t, ct, name, "no clean path")
	}
	for _, name := range []string{".bashrc", ".codex/sessions/x.md", ".config/other/x.md"} {
		refusedTarget(t, ct, name, "outside the configuration")
	}
	// a home reached through a link is no home
	linked := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(home, linked); err != nil {
		t.Fatal(err)
	}
	refusedTarget(t, ConfigTargets{Home: linked}, ruleTarget, "holds a link")
}

// An executable, a program by its name or its #! line, a file of another owner, with another hard link, or
// that is no text is never a target.
func TestExecutableAndNonTextFilesAreNeverTargets(t *testing.T) {
	home := fakeHome(t)
	ct := ConfigTargets{Home: home}
	write(t, filepath.Join(home, ".config/agents/EXEC.md"), "marker-c0ffee\n")
	if err := os.Chmod(filepath.Join(home, ".config/agents/EXEC.md"), 0o750); err != nil {
		t.Fatal(err)
	}
	refusedTarget(t, ct, ".config/agents/EXEC.md", "executable")
	write(t, filepath.Join(home, ".config/agents/SHEBANG.md"), "#!/bin/sh\necho marker-c0ffee\n")
	refusedTarget(t, ct, ".config/agents/SHEBANG.md", "a program")
	for _, name := range []string{".config/agents/x.sh", ".config/agents/procedures/x.py", ".claude/hooks/guard.json", ".claude/mods/m/README.md"} {
		refusedTarget(t, ConfigTargets{}, name, "a program")
	}
	write(t, filepath.Join(home, ".config/agents/NUL.md"), "marker-c0ffee \x00\n")
	refusedTarget(t, ct, ".config/agents/NUL.md", "not text")
	write(t, filepath.Join(home, ".config/agents/BIG.md"), strings.Repeat("x", MaxConfigFileBytes+1))
	refusedTarget(t, ct, ".config/agents/BIG.md", "limit")
	write(t, filepath.Join(home, ".config/agents/notes/RULES.md"), "marker-c0ffee linked twice\n")
	if err := os.Link(filepath.Join(home, ".config/agents/notes/RULES.md"), filepath.Join(home, ".config/agents/HARD.md")); err != nil {
		t.Fatal(err)
	}
	refusedTarget(t, ct, ".config/agents/HARD.md", "hard link")
	refusedTarget(t, ct, ".config/agents/NEW.md", "not there")
	targetUID = func() int { return os.Getuid() + 1 }
	defer func() { targetUID = os.Getuid }()
	refusedTarget(t, ct, ruleTarget, "not owned")
}

// systemd units are never targets: anything under a unit directory, a unit's drop-in, a quadlet, and a unit file
// wherever it lies.
func TestSystemdUnitsAreNeverTargets(t *testing.T) {
	for _, name := range []string{".config/systemd/user/momentum.service", ".config/systemd/user/x.md", ".config/agents/x.timer",
		".config/agents/x.service.d/override.conf", ".config/containers/systemd/x.container", ".config/agents/systemd/x.md"} {
		refusedTarget(t, ConfigTargets{}, name, "a systemd unit")
	}
}

// A configuration change edits its target's content and adds tests under tests/, nothing else: no other file, no
// mode change, no deletion or rename, and the new content stays text. Its test must fail on the file as it is.
func TestAConfigurationChangeEditsOnlyItsTargetsContent(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	home := fakeHome(t)
	s := configShip(t, st, home)
	c := testingCase(t, st)
	p, err := s.PrepareConfig(ctx, c.ID, ruleTarget, "")
	if err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(p.Work); len(entries) != 2 { // .config and .git
		t.Fatalf("the build tree holds more than its target: %v", entries)
	}
	target := filepath.Join(p.Work, ruleTarget)
	write(t, filepath.Join(p.Work, "tests/test_rules.py"), ruleTest)
	write(t, filepath.Join(p.Work, ".config/agents/zz-other.md"), "marker-c0ffee other\n")
	write(t, target, newRule)
	if _, err := checkPatch(t, s, ctx, c.ID, "python3 tests/test_rules.py", nil); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "neither the target") {
		t.Fatalf("a second file: %v", err)
	}
	os.Remove(filepath.Join(p.Work, ".config/agents/zz-other.md"))
	if err := os.Chmod(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := checkPatch(t, s, ctx, c.ID, "python3 tests/test_rules.py", nil); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("a mode change: %v", err)
	}
	os.Chmod(target, 0o644)
	if err := os.Rename(target, filepath.Join(p.Work, "tests/zz-rules.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := checkPatch(t, s, ctx, c.ID, "python3 tests/test_rules.py", nil); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "deletes or renames") {
		t.Fatalf("a rename: %v", err)
	}
	os.Remove(filepath.Join(p.Work, "tests/zz-rules.md"))
	if _, err := checkPatch(t, s, ctx, c.ID, "python3 tests/test_rules.py", nil); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "deletes or renames") {
		t.Fatalf("a deletion: %v", err)
	}
	write(t, target, "#!/bin/sh\nmarker-c0ffee new rule\n")
	if _, err := checkPatch(t, s, ctx, c.ID, "python3 tests/test_rules.py", nil); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "no configuration text") {
		t.Fatalf("a program as the new content: %v", err)
	}
	write(t, target, newRule)
	write(t, filepath.Join(p.Work, "tests/test_any.py"), "pass\n")
	if _, err := checkPatch(t, s, ctx, c.ID, "python3 tests/test_any.py", nil); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "passes without the change") {
		t.Fatalf("a test that passes on the file as it is: %v", err)
	}
	os.Remove(filepath.Join(p.Work, "tests/test_any.py"))
	os.Remove(filepath.Join(p.Work, "tests/test_rules.py"))
	write(t, filepath.Join(p.Work, "check_test.py"), ruleTest)
	if _, err := checkPatch(t, s, ctx, c.ID, "python3 check_test.py", []string{"check_test.py"}); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "under tests/") {
		t.Fatalf("a test outside tests/: %v", err)
	}
}

// An investigation names a configuration file as its fix's target; a report naming a repository too names neither,
// and a path outside the home names nothing.
func TestAnInvestigationNamesAConfigurationTarget(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	r.Ship = &Ship{ConfigTargets: ConfigTargets{Enabled: false, Allow: []string{ruleTarget}}} // a ship, with targets off
	// with configuration targets off, a report's config names nothing and its repository stands, and the schema and the
	// question name no configuration file
	off := openCase(t, st, "failures", "configuration-off", 1)
	report := `{"mechanism":"one cause","fix":"one fix","summary":"marker-c0ffee","config":"~/.config/agents/zz-rules.md","repo":"main"}`
	if err := r.keepFindings(ctx, store.LoopRun{Case: off.ID, CaseEpisode: off.Episode, ID: "fixture"}, nativeFinding(Result{Report: json.RawMessage(report)}), true, ""); err != nil {
		t.Fatal(err)
	}
	off, _ = st.CaseByID(ctx, off.ID)
	if repo, config, err := r.caseTarget(ctx, off); err != nil || repo != "main" || config != "" {
		t.Fatalf("switch off: %q %q %v", repo, config, err)
	}
	if r.investigationSchemaFor(off) != investigationSchema || strings.Contains(investigationSchema, "config") {
		t.Fatal("the schema offers config while configuration targets are off")
	}
	if q := r.investigationQuestion(off); q != liveQuestion || strings.Contains(q, "/ro/config") {
		t.Fatalf("the question names a configuration file while configuration targets are off: %s", q)
	}
	r.Ship = &Ship{ConfigTargets: ConfigTargets{Enabled: true}}
	if !strings.Contains(r.investigationSchemaFor(off), `"config"`) || !strings.Contains(r.investigationQuestion(off), "/ro/config") {
		t.Fatal("the schema or the question does not offer config while configuration targets are on")
	}
	for i, tc := range []struct{ report, repo, config string }{
		{`{"mechanism":"one cause","fix":"one fix","summary":"marker-c0ffee","config":"/ro/config/.config/agents/zz-rules.md"}`, "", ruleTarget},
		{`{"mechanism":"one cause","fix":"one fix","summary":"marker-c0ffee","config":"~/.config/agents/zz-rules.md","repo":"main"}`, "", ""},
		{`{"mechanism":"one cause","fix":"one fix","summary":"marker-c0ffee","config":"/etc/marker-c0ffee"}`, "", ""},
	} {
		c := openCase(t, st, "failures", fmt.Sprintf("configuration-%d", i), 1)
		run := store.LoopRun{Case: c.ID, CaseEpisode: c.Episode, ID: "fixture"}
		if err := r.keepFindings(ctx, run, nativeFinding(Result{Report: json.RawMessage(tc.report)}), true, ""); err != nil {
			t.Fatal(err)
		}
		c, _ = st.CaseByID(ctx, c.ID)
		if repo, config, err := r.caseTarget(ctx, c); err != nil || repo != tc.repo || config != tc.config {
			t.Fatalf("%s: %q %q %v", tc.report, repo, config, err)
		}
	}
}

// A testing case whose investigation named a configuration file builds a configuration package from the file the
// run's copy held, and is held while configuration targets are off or allow does not name its file.
func TestABuildForAConfigurationCaseMountsItsTargetsTree(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	home := fakeHome(t)
	r := localRunner(t, st)
	r.Ship = configShip(t, st, home)
	r.Ship.Dir = r.Dir
	r.pass = &passInputs{}
	c := testingCase(t, st)
	raw, _ := json.Marshal(map[string]any{"repo": "", "config": ruleTarget, "episode": c.Episode})
	if err := st.CaseNote(ctx, c.ID, repoAction, string(raw), "fixture"); err != nil {
		t.Fatal(err)
	}
	r.Ship.ConfigTargets.Enabled = false
	if _, step, why, err := r.pick(ctx, "failures", c.ID, true); err != nil || step == StepBuild || !strings.Contains(why, "configuration targets are off") {
		t.Fatalf("picked with the switch off: %q %q %v", step, why, err)
	}
	r.Ship.ConfigTargets.Enabled, r.Ship.ConfigTargets.Allow = true, []string{".config/agents/zz-other.md"}
	if _, step, why, err := r.pick(ctx, "failures", c.ID, true); err != nil || step == StepBuild || !strings.Contains(why, "allow names") {
		t.Fatalf("picked a file allow does not name: %q %q %v", step, why, err)
	}
	r.Ship.ConfigTargets.Allow = []string{ruleTarget}
	if _, step, _, err := r.pick(ctx, "failures", c.ID, true); err != nil || step != StepBuild {
		t.Fatalf("pick: %q %v", step, err)
	}
	run := store.LoopRun{ID: "fixture-build", Case: c.ID, Dir: filepath.Join(r.Dir, "runs", "fixture-build"), CaseEpisode: c.Episode}
	if _, err := r.buildPackage(ctx, c, run); !errors.Is(err, ErrRefused) || held(err) || !strings.Contains(err.Error(), "left out") {
		t.Fatalf("a target outside the copy: %v", err)
	}
	r.pass.manifest.Configuration.Files = []ConfigFile{{Path: ruleTarget, Root: ".config/agents", SHA256: hashOf([]byte("marker-c0ffee other\n"))}}
	if _, err := r.buildPackage(ctx, c, run); !held(err) || !strings.Contains(err.Error(), "since the run's configuration copy") {
		t.Fatalf("a target changed since the copy: %v", err)
	}
	r.pass.manifest.Configuration.Files = []ConfigFile{{Path: ruleTarget, Root: ".config/agents", SHA256: hashOf([]byte(oldRule))}}
	p, err := r.buildPackage(ctx, c, run)
	if err != nil || p.Kind != KindConfig || p.Work != filepath.Join(run.Dir, "work") || p.TargetBase != hashOf([]byte(oldRule)) {
		t.Fatalf("build package: %+v %v", p, err)
	}
	if b, err := os.ReadFile(filepath.Join(p.Work, ruleTarget)); err != nil || string(b) != oldRule {
		t.Fatalf("the build tree holds %q %v", b, err)
	}
}

// proposedConfigCase is a configuration case proposed through the runner: its investigation named the rules file, the
// build changed it with a discriminating test, and the host checked and proposed it.
func proposedConfigCase(t *testing.T) (*Runner, store.Case, string, Package) {
	t.Helper()
	ctx := context.Background()
	st := testStore(t)
	home := fakeHome(t)
	r := localRunner(t, st)
	r.Ship = configShip(t, st, home)
	r.Ship.Dir = r.Dir
	r.pass = &passInputs{}
	r.pass.manifest.Configuration.Files = []ConfigFile{{Path: ruleTarget, Root: ".config/agents", SHA256: hashOf([]byte(oldRule))}}
	c := testingCase(t, st)
	raw, _ := json.Marshal(map[string]any{"repo": "", "config": ruleTarget, "episode": c.Episode})
	if err := st.CaseNote(ctx, c.ID, repoAction, string(raw), "fixture"); err != nil {
		t.Fatal(err)
	}
	run := store.LoopRun{ID: "fixture-build", Case: c.ID, Dir: filepath.Join(r.Dir, "runs", "fixture-build"), CaseEpisode: c.Episode}
	p, err := r.buildPackage(ctx, c, run)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Work, ruleTarget), newRule)
	write(t, filepath.Join(p.Work, "tests/test_rules.py"), ruleTest)
	if _, err := r.Ship.CheckArgs(ctx, c.ID, []string{"python3", "tests/test_rules.py"}, nil, fixturePatch(t, r.Ship, c.ID)); err != nil {
		t.Fatal(err)
	}
	if p, err = proposePatch(t, r.Ship, ctx, c.ID, configProposal); err != nil || p.State != Proposed {
		t.Fatalf("propose: %+v %v", p, err)
	}
	return r, c, home, p
}

// A proposal whose file comes to hold neither the content it was built on nor its change is withdrawn: its note
// resolves, the case log says why, and the case builds again on the file as it is. Its record and patch stay.
func TestAConfigurationProposalWhoseFileMovedIsWithdrawn(t *testing.T) {
	ctx := context.Background()
	r, c, home, p := proposedConfigCase(t)
	edit := "# Rules\n\nmarker-c0ffee the owner's edit\n"
	write(t, filepath.Join(home, ruleTarget), edit)
	if got, step, _, err := r.pick(ctx, "failures", c.ID, false); err != nil || got.ID != c.ID || step != StepBuild {
		t.Fatalf("pick after the edit: %q %q %v", got.ID, step, err)
	}
	if got, _, _ := r.Ship.Load(c.ID); got.State != Withdrawn {
		t.Fatalf("the proposal over an edited file: %s", got.State)
	}
	if n, ok := noteByID(t, r.Store, configNoteID(c.ID)); ok && n.Done.IsZero() {
		t.Fatalf("a note stands after the withdrawal: %+v", n)
	}
	log, _ := r.Store.CaseLog(ctx, c.ID)
	if !logHas(log, "configuration proposal withdrawn", "holds something other than") {
		t.Fatalf("the case log says nothing of the withdrawal: %+v", log)
	}
	if got, _ := r.Store.CaseByID(ctx, c.ID); got.Status != store.CaseTesting {
		t.Fatalf("the case after the withdrawal: %s", got.Status)
	}
	// the next build is on the file as it is, and the withdrawn proposal's record and patch stay where its note named them
	r.pass.manifest.Configuration.Files = []ConfigFile{{Path: ruleTarget, Root: ".config/agents", SHA256: hashOf([]byte(edit))}}
	run := store.LoopRun{ID: "fixture-build-2", Case: c.ID, Dir: filepath.Join(r.Dir, "runs", "fixture-build-2"), CaseEpisode: c.Episode}
	next, err := r.buildPackage(ctx, c, run)
	if err != nil || next.State != Prepared || next.TargetBase != hashOf([]byte(edit)) {
		t.Fatalf("the build after the withdrawal: %+v %v", next, err)
	}
	if b, err := os.ReadFile(filepath.Join(next.Work, ruleTarget)); err != nil || string(b) != edit {
		t.Fatalf("the next build tree holds %q %v", b, err)
	}
	for _, ext := range []string{".json", ".patch"} {
		if _, err := os.Stat(r.Ship.proposalFile(p, ext)); err != nil {
			t.Fatalf("the withdrawn proposal's %s: %v", ext, err)
		}
	}
	// a file that became a link withdraws a proposal too
	r2, c2, home2, _ := proposedConfigCase(t)
	elsewhere := filepath.Join(t.TempDir(), "zz-rules.md")
	write(t, elsewhere, oldRule)
	os.Remove(filepath.Join(home2, ruleTarget))
	if err := os.Symlink(elsewhere, filepath.Join(home2, ruleTarget)); err != nil {
		t.Fatal(err)
	}
	if got, err := r2.Ship.reconcileConfig(ctx, c2.ID); err != nil || got.State != Withdrawn {
		t.Fatalf("a proposal whose file became a link: %+v %v", got, err)
	}
}

func noteByID(t *testing.T, st *store.Store, id string) (store.Note, bool) {
	t.Helper()
	notes, err := st.Notes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range notes {
		if n.ID == id {
			return n, true
		}
	}
	return store.Note{}, false
}

func logHas(log []store.CaseEntry, action, text string) bool {
	for _, e := range log {
		if e.Action == action && strings.Contains(e.Detail, text) {
			return true
		}
	}
	return false
}

// A configuration path longer than a record holds names nothing: the case waits for a target, and selection goes on.
// A record that cannot be read holds only its case.
func TestALongConfigurationPathNamesNothing(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	r.Ship = &Ship{ConfigTargets: ConfigTargets{Enabled: true, Allow: []string{ruleTarget}}, Store: st, Dir: r.Dir}
	c := openCase(t, st, "failures", "configuration-long", 1)
	long := "/ro/config/.config/agents/" + strings.Repeat("ab/", 700) + "zz-rules.md"
	report := `{"mechanism":"one cause","fix":"one fix","summary":"marker-c0ffee","config":"` + long + `"}`
	if err := r.keepFindings(ctx, store.LoopRun{Case: c.ID, CaseEpisode: c.Episode, ID: "fixture"}, nativeFinding(Result{Report: json.RawMessage(report)}), true, ""); err != nil {
		t.Fatal(err)
	}
	c, _ = st.CaseByID(ctx, c.ID)
	if repo, config, err := r.caseTarget(ctx, c); err != nil || repo != "" || config != "" {
		t.Fatalf("a path longer than a record holds: %q %q %v", repo, config, err)
	}
	broken := testingCase(t, st)
	if err := st.CaseNote(ctx, broken.ID, repoAction, `{"repo":"","config":".config/agents/zz-rules.md","epis`, "fixture"); err != nil {
		t.Fatal(err)
	}
	if _, _, why, err := r.pick(ctx, "failures", "", false); err != nil || !strings.Contains(why, broken.ID) {
		t.Fatalf("a record that cannot be read: %q %v", why, err)
	}
}
