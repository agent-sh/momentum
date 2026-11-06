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
	"golang.org/x/sys/unix"
)

const (
	ruleTarget = ".config/agents/RULES.md"
	oldRule    = "# Rules\n\nmarker-c0ffee old rule\n"
	newRule    = "# Rules\n\nmarker-c0ffee new rule\n"
	ruleTest   = "import sys\ntext = open('.config/agents/RULES.md').read()\nsys.exit(0 if 'marker-c0ffee new rule' in text else 1)\n"
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

var configProposal = Proposal{Title: "Rules say the new rule", Body: "The old rule caused the failure.", Rollback: "momentum case rollback restores the journaled file",
	Lesson: "Rules say the new rule.", Signal: goodSignal, Window: 7 * 24 * time.Hour}

// proposedConfig prepares, checks and proposes the rules change.
func proposedConfig(t *testing.T, s *Ship, c store.Case) Package {
	t.Helper()
	ctx := context.Background()
	p, err := s.PrepareConfig(ctx, c.ID, "/ro/config/"+ruleTarget, "")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Work, ruleTarget), newRule)
	write(t, filepath.Join(p.Work, "tests/test_rules.py"), ruleTest)
	if p, err = checkPatch(t, s, ctx, c.ID, "python3 tests/test_rules.py", nil); err != nil || p.Check.Before.Exit == 0 || p.Check.After.Exit != 0 {
		t.Fatalf("check: %+v %v", p.Check, err)
	}
	if p, err = proposePatch(t, s, ctx, c.ID, configProposal); err != nil || p.State != Proposed || p.PR != "" {
		t.Fatalf("propose: %+v %v", p, err)
	}
	return p
}

// reviewedConfig is a proposed rules change with a clean review posted to its case's log.
func reviewedConfig(t *testing.T, s *Ship, st *store.Store, c store.Case) Package {
	t.Helper()
	p := proposedConfig(t, s, c)
	r := &Runner{Store: st, Ship: s, Dir: s.Dir}
	p, err := s.Reviewed(context.Background(), c.ID, Review{Head: p.Head, Text: "Checked every changed file."})
	if err != nil {
		t.Fatal(err)
	}
	if p, err = r.postReview(context.Background(), p); err != nil || !p.Clean() {
		t.Fatalf("post: %+v %v", p, err)
	}
	return p
}

func readTarget(t *testing.T, home string) (string, os.FileMode) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, ruleTarget))
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Lstat(filepath.Join(home, ruleTarget))
	if err != nil {
		t.Fatal(err)
	}
	return string(b), st.Mode()
}

// refusedTarget asserts that a target is refused with why in its cause, by its name and when allow names it exactly.
func refusedTarget(t *testing.T, ct ConfigTargets, name, why string) {
	t.Helper()
	ct.Allow = append(append([]string(nil), ct.Allow...), name)
	_, err := ct.admit(name)
	if err == nil && ct.Home != "" {
		var d *targetDir
		if d, err = ct.open(name, false); err == nil {
			_, err = d.state()
			d.Close()
		}
	}
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), why) {
		t.Errorf("%s: want a refusal for %q, got %v", name, why, err)
	}
}

// allow names exact files: an empty list admits nothing, and a directory or a pattern admits nothing either.
func TestAnEmptyOrInexactAllowAdmitsNothing(t *testing.T) {
	if _, err := (ConfigTargets{}).admit(ruleTarget); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "names no file") {
		t.Fatalf("an empty allow: %v", err)
	}
	for _, entry := range []string{".config/agents", ".config/agents/", "~/.config/agents", ".config/agents/*.md", ".config/agents/R?LES.md",
		".config/agents/[R]ULES.md", ".config/agents/**", "/ro/config/" + ruleTarget, ".config/agents/OTHER.md"} {
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

// The loop's own configuration, rules and policy are never targets, whatever allow says: momentum's configuration,
// every file loop.rules names (and the file one resolves to), the scheduled-run policy.
func TestTheLoopsOwnFilesAreNeverTargets(t *testing.T) {
	home := fakeHome(t)
	write(t, filepath.Join(home, ".config/agents/scheduled-ai-policy.md"), "marker-c0ffee policy\n")
	write(t, filepath.Join(home, ".config/agents/SHARED.md"), "marker-c0ffee shared\n")
	write(t, filepath.Join(home, ".config/agents/procedures/prs.md"), "marker-c0ffee procedure\n")
	write(t, filepath.Join(home, ".config/momentum/sources.json"), "{}\n")
	// a rules entry that is a link: its file is the loop's own too
	if err := os.Symlink("procedures/prs.md", filepath.Join(home, ".config/agents/LINKED.md")); err != nil {
		t.Fatal(err)
	}
	ct := ConfigTargets{Home: home, Own: []string{"~/.config/agents/SHARED.md", filepath.Join(home, ".config/agents/LINKED.md")}}
	for _, name := range []string{".config/agents/scheduled-ai-policy.md", ".config/agents/SHARED.md", ".config/agents/procedures/prs.md",
		".config/momentum/sources.json", ".config/agents/shared.md"} {
		refusedTarget(t, ct, name, "the loop's own")
	}
	if _, err := (ConfigTargets{Home: home, Allow: []string{ruleTarget}, Own: ct.Own}).admit(ruleTarget); err != nil {
		t.Fatalf("another file: %v", err)
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
	for _, name := range []string{"/etc/passwd", "../x.md", ".config/agents/../../x.md", "~/../x.md", ".config/agents/./RULES.md", ""} {
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
	write(t, filepath.Join(home, ".config/agents/SHARED.md"), "marker-c0ffee linked twice\n")
	if err := os.Link(filepath.Join(home, ".config/agents/SHARED.md"), filepath.Join(home, ".config/agents/HARD.md")); err != nil {
		t.Fatal(err)
	}
	refusedTarget(t, ct, ".config/agents/HARD.md", "hard link")
	refusedTarget(t, ct, ".config/agents/NEW.md", "not there")
	targetUID = func() int { return os.Getuid() + 1 }
	defer func() { targetUID = os.Getuid }()
	refusedTarget(t, ct, ruleTarget, "not owned")
}

// A configuration change goes through the repository package's gates: a check that fails before and passes after, a
// clean review posted (to the case's log), and a landing only while the switch is on and the loop is live. The
// previous content, hash and mode are journaled before the write, the file keeps its mode, the change is in effect
// while the file holds what landed, and the owner's case rollback restores the file.
func TestAConfigurationChangeShipsCheckedJournaledAndRolledBack(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	home := fakeHome(t)
	s := configShip(t, st, home)
	c := testingCase(t, st)
	p := proposedConfig(t, s, c)
	if !strings.Contains(strings.Join(p.Changed, " "), "tests/test_rules.py") || p.Kind != KindConfig || p.Target != ruleTarget {
		t.Fatalf("package: %+v", p)
	}
	if _, err := s.Land(ctx, c.ID); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "review") {
		t.Fatalf("landed unreviewed: %v", err)
	}
	r := &Runner{Store: st, Ship: s, Dir: s.Dir}
	p, err := s.Reviewed(ctx, c.ID, Review{Head: p.Head, Text: "Checked every changed file.", Findings: []Finding{{Severity: "medium", Where: ruleTarget, Problem: "y", Fix: "z"}}})
	if err != nil {
		t.Fatal(err)
	}
	if p, err = r.postReview(ctx, p); err != nil || p.State != Revising || !strings.HasPrefix(p.Reviews[0].Comment, "case log") {
		t.Fatalf("a medium review: %+v %v", p, err)
	}
	write(t, filepath.Join(p.Work, ruleTarget), newRule+"\nmarker-c0ffee kept\n")
	if _, err := checkPatch(t, s, ctx, c.ID, "python3 tests/test_rules.py", nil); err != nil {
		t.Fatal(err)
	}
	if p, err = proposePatch(t, s, ctx, c.ID, configProposal); err != nil {
		t.Fatal(err)
	}
	if p, err = s.Reviewed(ctx, c.ID, Review{Head: p.Head, Text: "Checked every changed file."}); err != nil {
		t.Fatal(err)
	}
	if p, err = r.postReview(ctx, p); err != nil || !p.Clean() {
		t.Fatal(err)
	}
	s.ConfigTargets.Enabled = false
	if _, err := s.Land(ctx, c.ID); !held(err) || !strings.Contains(err.Error(), "config_targets") {
		t.Fatalf("landed while the switch is off: %v", err)
	}
	if done := r.reviews(ctx, ""); len(done) != 1 || !strings.Contains(done[0].Skipped, "config_targets") {
		t.Fatalf("the pass with the switch off: %+v", done)
	}
	s.ConfigTargets.Enabled, s.Live = true, false
	if _, err := s.Land(ctx, c.ID); !held(err) || !strings.Contains(err.Error(), "not live") {
		t.Fatalf("landed while not live: %v", err)
	}
	if got, mode := readTarget(t, home); got != oldRule || mode.Perm() != 0o640 {
		t.Fatalf("a refused landing wrote the file: %q %v", got, mode)
	}
	if _, err := os.Stat(filepath.Join(s.pkgDir(c.ID), configJournal)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused landing journaled: %v", err)
	}
	s.Live = true
	landed := newRule + "\nmarker-c0ffee kept\n"
	if p, err = s.Land(ctx, c.ID); err != nil || p.State != Landed || p.TargetLanded != hashOf([]byte(landed)) {
		t.Fatalf("land: %+v %v", p, err)
	}
	if got, mode := readTarget(t, home); got != landed || mode != 0o640 {
		t.Fatalf("landed %q %v", got, mode)
	}
	if entries, _ := os.ReadDir(filepath.Join(home, ".config/agents")); len(entries) != 1 {
		t.Fatalf("left beside it: %v", entries)
	}
	if got, _ := st.CaseByID(ctx, c.ID); got.Status != store.CaseDeployed {
		t.Fatalf("the case after landing: %s", got.Status)
	}
	info, err := os.Stat(filepath.Join(s.pkgDir(c.ID), configJournal))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("journal: %v %v", info, err)
	}
	var j ConfigJournal
	if ok, err := readRecord(s.pkgDir(c.ID), configJournal, &j); !ok || err != nil || string(j.Old.Content) != oldRule ||
		j.Old.SHA256 != hashOf([]byte(oldRule)) || j.Old.Mode != 0o640 || j.Old.UID != uint32(os.Getuid()) {
		t.Fatalf("journal: %+v %v", j, err)
	}
	if _, why, err := s.InEffect(ctx, &p); err != nil || why != "" {
		t.Fatalf("in effect: %q %v", why, err)
	}
	// the owner's rollback, switch off and not live: the file gets its old content in the mode it has now
	s.ConfigTargets.Enabled, s.Live = false, false
	if err := os.Chmod(filepath.Join(home, ruleTarget), 0o600); err != nil {
		t.Fatal(err)
	}
	if p, err = s.RollBack(ctx, c.ID, "it did nothing"); err != nil || p.State != RolledBack {
		t.Fatalf("rollback: %+v %v", p, err)
	}
	if got, mode := readTarget(t, home); got != oldRule || mode != 0o600 {
		t.Fatalf("the rollback left %q %v", got, mode)
	}
	if got, _ := st.CaseByID(ctx, c.ID); got.Status != store.CaseTesting {
		t.Fatalf("the case after the rollback: %s", got.Status)
	}
	if p, err = s.RollBack(ctx, c.ID, "again"); err != nil || p.State != RolledBack {
		t.Fatalf("a second rollback is its receipt: %+v %v", p, err)
	}
}

// A file changed since the proposal is a hold, not an overwrite: the landing writes nothing and journals nothing, and
// the package waits. A file changed since its change landed holds the rollback the same way.
func TestAFileChangedSinceTheProposalIsAHold(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	home := fakeHome(t)
	s := configShip(t, st, home)
	c := testingCase(t, st)
	reviewedConfig(t, s, st, c)
	edit := "# Rules\n\nmarker-c0ffee the owner's edit\n"
	write(t, filepath.Join(home, ruleTarget), edit)
	p, err := s.Land(ctx, c.ID)
	if !held(err) || !strings.Contains(err.Error(), "changed since the proposal") {
		t.Fatalf("landed over the owner's edit: %v", err)
	}
	if got, _ := readTarget(t, home); got != edit {
		t.Fatalf("the owner's edit became %q", got)
	}
	if p, _, _ = s.Load(c.ID); p.State != Proposed {
		t.Fatalf("the held package: %s", p.State)
	}
	if _, err := os.Stat(filepath.Join(s.pkgDir(c.ID), configJournal)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a held landing journaled: %v", err)
	}
	// the owner puts the file back: the change lands, and a later edit holds its rollback
	write(t, filepath.Join(home, ruleTarget), oldRule)
	if p, err = s.Land(ctx, c.ID); err != nil || p.State != Landed {
		t.Fatalf("land: %+v %v", p, err)
	}
	write(t, filepath.Join(home, ruleTarget), edit)
	if _, err := s.RollBack(ctx, c.ID, "no effect"); !held(err) || !strings.Contains(err.Error(), "changed since its change landed") {
		t.Fatalf("rolled back over a changed file: %v", err)
	}
	if got, _ := readTarget(t, home); got != edit {
		t.Fatalf("the held rollback wrote %q", got)
	}
}

// The landing resolves the path again and checks every refusal at landing time, not only at the proposal.
func TestALandingChecksEveryRefusalAgain(t *testing.T) {
	path := func(home string) string { return filepath.Join(home, ruleTarget) }
	for name, tc := range map[string]struct {
		break_, restore func(t *testing.T, s *Ship, home string)
		why             string
	}{
		"a link": {func(t *testing.T, s *Ship, home string) {
			os.Rename(path(home), path(home)+".real")
			os.Symlink("RULES.md.real", path(home))
		}, func(t *testing.T, s *Ship, home string) {
			os.Remove(path(home))
			os.Rename(path(home)+".real", path(home))
		}, "a link"},
		"executable": {func(t *testing.T, s *Ship, home string) { os.Chmod(path(home), 0o750) },
			func(t *testing.T, s *Ship, home string) { os.Chmod(path(home), 0o640) }, "executable"},
		"narrowed allow": {func(t *testing.T, s *Ship, home string) { s.ConfigTargets.Allow = []string{".config/agents/OTHER.md"} },
			func(t *testing.T, s *Ship, home string) { s.ConfigTargets.Allow = []string{ruleTarget} }, "allow"},
		"the loop's own": {func(t *testing.T, s *Ship, home string) { s.ConfigTargets.Own = []string{"~/" + ruleTarget} },
			func(t *testing.T, s *Ship, home string) { s.ConfigTargets.Own = nil }, "the loop's own"},
		"a directory link": {func(t *testing.T, s *Ship, home string) {
			agents := filepath.Join(home, ".config/agents")
			os.Rename(agents, agents+".real")
			os.Symlink("agents.real", agents)
		}, func(t *testing.T, s *Ship, home string) {
			agents := filepath.Join(home, ".config/agents")
			os.Remove(agents)
			os.Rename(agents+".real", agents)
		}, "a link"},
	} {
		t.Run(name, func(t *testing.T) {
			st := testStore(t)
			ctx := context.Background()
			home := fakeHome(t)
			s := configShip(t, st, home)
			c := testingCase(t, st)
			reviewedConfig(t, s, st, c)
			tc.break_(t, s, home)
			if _, err := s.Land(ctx, c.ID); !held(err) || !strings.Contains(err.Error(), tc.why) {
				t.Fatalf("landed: %v", err)
			}
			if b, _ := os.ReadFile(path(home)); string(b) != oldRule {
				t.Fatalf("a refused landing wrote %q", b)
			}
			if _, err := os.Stat(filepath.Join(s.pkgDir(c.ID), configJournal)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a refused landing journaled: %v", err)
			}
			tc.restore(t, s, home)
			if p, err := s.Land(ctx, c.ID); err != nil || p.State != Landed {
				t.Fatalf("land after the restore: %+v %v", p, err)
			}
		})
	}
}

// A landing keeps the mode, owner and group the file has at landing time, whatever they were at the proposal.
func TestALandingKeepsTheModeAtLandingTime(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	home := fakeHome(t)
	s := configShip(t, st, home)
	c := testingCase(t, st)
	reviewedConfig(t, s, st, c)
	if err := os.Chmod(filepath.Join(home, ruleTarget), 0o600); err != nil {
		t.Fatal(err)
	}
	if p, err := s.Land(ctx, c.ID); err != nil || p.State != Landed {
		t.Fatalf("land: %+v %v", p, err)
	}
	if got, mode := readTarget(t, home); got != newRule || mode != 0o600 {
		t.Fatalf("landed %q %v", got, mode)
	}
}

// A configuration change edits its target's content and adds tests under tests/, nothing else: no other file, no
// mode change, no deletion or rename, and the new content stays text.
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
		t.Fatalf("the clone holds more than its target: %v", entries)
	}
	target := filepath.Join(p.Work, ruleTarget)
	write(t, filepath.Join(p.Work, "tests/test_rules.py"), ruleTest)
	write(t, filepath.Join(p.Work, ".config/agents/OTHER.md"), "marker-c0ffee other\n")
	write(t, target, newRule)
	if _, err := checkPatch(t, s, ctx, c.ID, "python3 tests/test_rules.py", nil); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "neither the target") {
		t.Fatalf("a second file: %v", err)
	}
	os.Remove(filepath.Join(p.Work, ".config/agents/OTHER.md"))
	if err := os.Chmod(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := checkPatch(t, s, ctx, c.ID, "python3 tests/test_rules.py", nil); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("a mode change: %v", err)
	}
	os.Chmod(target, 0o644)
	if err := os.Rename(target, filepath.Join(p.Work, "tests/RULES.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := checkPatch(t, s, ctx, c.ID, "python3 tests/test_rules.py", nil); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "deletes or renames") {
		t.Fatalf("a rename: %v", err)
	}
	os.Remove(filepath.Join(p.Work, "tests/RULES.md"))
	if _, err := checkPatch(t, s, ctx, c.ID, "python3 tests/test_rules.py", nil); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "deletes or renames") {
		t.Fatalf("a deletion: %v", err)
	}
	write(t, target, "#!/bin/sh\nmarker-c0ffee new rule\n")
	if _, err := checkPatch(t, s, ctx, c.ID, "python3 tests/test_rules.py", nil); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "no configuration text") {
		t.Fatalf("a program as the new content: %v", err)
	}
	write(t, target, newRule)
	os.Remove(filepath.Join(p.Work, "tests/test_rules.py"))
	write(t, filepath.Join(p.Work, "check_test.py"), ruleTest)
	if _, err := checkPatch(t, s, ctx, c.ID, "python3 check_test.py", []string{"check_test.py"}); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "under tests/") {
		t.Fatalf("a test outside tests/: %v", err)
	}
}

// The landing journals the previous content before it writes, and a landing a crash interrupted is finished from that
// journal: written again when the file still holds the old content, finished when it holds the new, and held, with
// the owner's edit kept, when it holds neither.
func TestAnInterruptedConfigurationLandingRecovers(t *testing.T) {
	for _, stage := range []string{"journaled", "written", "drifted"} {
		t.Run(stage, func(t *testing.T) {
			st := testStore(t)
			ctx := context.Background()
			home := fakeHome(t)
			s := configShip(t, st, home)
			c := testingCase(t, st)
			reviewedConfig(t, s, st, c)
			crash := errors.New("crash")
			at := stage
			if stage == "drifted" {
				at = "written"
			}
			s.ConfigStage = func(got string) error {
				if got != at {
					return nil
				}
				if got == "journaled" {
					var j ConfigJournal
					if ok, err := readRecord(s.pkgDir(c.ID), configJournal, &j); !ok || err != nil || string(j.Old.Content) != oldRule {
						t.Fatalf("journal before the write: %+v %v", j, err)
					}
					if got, _ := readTarget(t, home); got != oldRule {
						t.Fatal("written before its journal")
					}
				}
				return crash
			}
			if _, err := s.Land(ctx, c.ID); !errors.Is(err, crash) {
				t.Fatalf("land: %v", err)
			}
			s.ConfigStage = nil
			if p, _, _ := s.Load(c.ID); p.State != Landing {
				t.Fatalf("after the crash: %s", p.State)
			}
			if stage == "drifted" {
				// the change went in and the owner edited it after: the landing holds with its journal, and the owner's
				// rollback keeps his edit, names the journal and frees the file
				edit := "marker-c0ffee the owner's edit\n"
				write(t, filepath.Join(home, ruleTarget), edit)
				if _, err := s.Land(ctx, c.ID); !held(err) || !strings.Contains(err.Error(), "neither") || !strings.Contains(err.Error(), configJournal) {
					t.Fatalf("resumed over the owner's edit: %v", err)
				}
				if got, _ := readTarget(t, home); got != edit {
					t.Fatalf("the owner's edit became %q", got)
				}
				if p, _, _ := s.Load(c.ID); p.State != Landing {
					t.Fatalf("after the hold: %s", p.State)
				}
				if _, err := os.Stat(filepath.Join(s.pkgDir(c.ID), configJournal)); err != nil {
					t.Fatalf("the held landing's journal: %v", err)
				}
				keptOwnersEdit(t, s, st, c, home, edit)
				return
			}
			if stage == "written" {
				// the swap's scratch name left beside the file is cleared too
				write(t, filepath.Join(home, ".config/agents", swapName(c.ID)), oldRule)
			}
			s.Live = false
			if _, err := s.Land(ctx, c.ID); !held(err) {
				t.Fatalf("resumed while not live: %v", err)
			}
			s.Live = true
			p, err := s.Land(ctx, c.ID)
			if err != nil || p.State != Landed {
				t.Fatalf("resumed: %+v %v", p, err)
			}
			if got, mode := readTarget(t, home); got != newRule || mode != 0o640 {
				t.Fatalf("resumed to %q %v", got, mode)
			}
			if entries, _ := os.ReadDir(filepath.Join(home, ".config/agents")); len(entries) != 1 {
				t.Fatalf("left beside it: %v", entries)
			}
		})
	}
}

// A configuration change with no measured effect is rolled back by the measure step only while configuration targets
// are on: with the switch off the rollback is held and the file keeps the change.
func TestTheMeasureStepHoldsAConfigurationRollbackWhileTheSwitchIsOff(t *testing.T) {
	m := measuredCase(t)
	home := fakeHome(t)
	write(t, filepath.Join(home, ruleTarget), newRule)
	os.Chmod(filepath.Join(home, ruleTarget), 0o640)
	p, _, _ := m.r.Ship.Load(m.c.ID)
	p.Kind, p.Target, p.TargetLanded, p.Repo, p.Dir, p.PR = KindConfig, ruleTarget, hashOf([]byte(newRule)), "", "", ""
	p.TargetBase = &ConfigState{SHA256: hashOf([]byte(oldRule)), Mode: 0o640}
	if err := m.r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	j := ConfigJournal{Target: ruleTarget, Old: ConfigState{SHA256: hashOf([]byte(oldRule)), Mode: 0o640, Content: []byte(oldRule)},
		New: ConfigState{SHA256: hashOf([]byte(newRule))}, Head: p.Landed[0]}
	if err := writeDurable(m.r.Ship.pkgDir(m.c.ID), configJournal, j); err != nil {
		t.Fatal(err)
	}
	m.r.Ship.Live, m.r.Ship.ConfigTargets = true, ConfigTargets{Enabled: false, Allow: []string{ruleTarget}, Home: home}
	if got := m.measure(t, Options{}); got.Status != "measuring" {
		t.Fatalf("in effect: %+v", got)
	}
	effect := m.clock
	hours(t, m.st, "a", effect.Add(time.Minute), 23*time.Hour, 100, 6)
	m.clock = effect.Add(25 * time.Hour)
	if got := m.measure(t, Options{}); got.Status != "held" || !strings.Contains(got.Why, "config_targets") {
		t.Fatalf("switch off: %+v", got)
	}
	if got, _ := readTarget(t, home); got != newRule {
		t.Fatalf("a held rollback wrote %q", got)
	}
	if m.theCase(t).Status != store.CaseMeasuring {
		t.Fatal("a held rollback moved the case")
	}
	m.r.Ship.ConfigTargets.Enabled = true
	if got := m.measure(t, Options{}); got.Status != "rolled back" {
		t.Fatalf("switch on: %+v", got)
	}
	if got, mode := readTarget(t, home); got != oldRule || mode != 0o640 {
		t.Fatalf("the rollback left %q %v", got, mode)
	}
}

// An investigation names a configuration file as its fix's target; a report naming a repository too names neither,
// and a path outside the home names nothing.
func TestAnInvestigationNamesAConfigurationTarget(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	// with configuration targets off, a report's config names nothing and its repository stands
	off := openCase(t, st, "failures", "configuration-off", 1)
	report := `{"mechanism":"one cause","fix":"one fix","summary":"marker-c0ffee","config":"~/.config/agents/RULES.md","repo":"main"}`
	if err := r.keepFindings(ctx, store.LoopRun{Case: off.ID, CaseEpisode: off.Episode, ID: "fixture"}, nativeFinding(Result{Report: json.RawMessage(report)}), true, ""); err != nil {
		t.Fatal(err)
	}
	off, _ = st.CaseByID(ctx, off.ID)
	if repo, config, err := r.caseTarget(ctx, off); err != nil || repo != "main" || config != "" {
		t.Fatalf("switch off: %q %q %v", repo, config, err)
	}
	if r.investigationSchemaFor() != investigationSchema || strings.Contains(investigationSchema, "config") {
		t.Fatal("the schema offers config while configuration targets are off")
	}
	r.Ship = &Ship{ConfigTargets: ConfigTargets{Enabled: true}}
	if !strings.Contains(r.investigationSchemaFor(), `"config"`) {
		t.Fatal("the schema does not offer config while configuration targets are on")
	}
	for i, tc := range []struct{ report, repo, config string }{
		{`{"mechanism":"one cause","fix":"one fix","summary":"marker-c0ffee","config":"/ro/config/.config/agents/RULES.md"}`, "", ruleTarget},
		{`{"mechanism":"one cause","fix":"one fix","summary":"marker-c0ffee","config":"~/.config/agents/RULES.md","repo":"main"}`, "", ""},
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
// run's copy held, and waits while configuration targets are off.
func TestABuildForAConfigurationCaseMountsItsTargetsClone(t *testing.T) {
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
	if _, step, _, err := r.pick(ctx, "failures", c.ID, true); err != nil || step == StepBuild {
		t.Fatalf("picked with the switch off: %q %v", step, err)
	}
	r.Ship.ConfigTargets.Enabled = true
	if _, step, _, err := r.pick(ctx, "failures", c.ID, true); err != nil || step != StepBuild {
		t.Fatalf("pick: %q %v", step, err)
	}
	run := store.LoopRun{ID: "fixture-build", Case: c.ID, Dir: filepath.Join(r.Dir, "runs", "fixture-build"), CaseEpisode: c.Episode}
	if _, err := r.buildPackage(ctx, c, run); !held(err) || !strings.Contains(err.Error(), "left out") {
		t.Fatalf("a target outside the copy: %v", err)
	}
	r.pass.manifest.Configuration.Files = []ConfigFile{{Path: ruleTarget, Root: ".config/agents", SHA256: hashOf([]byte("marker-c0ffee other\n"))}}
	if _, err := r.buildPackage(ctx, c, run); !held(err) || !strings.Contains(err.Error(), "since the run's configuration copy") {
		t.Fatalf("a target changed since the copy: %v", err)
	}
	r.pass.manifest.Configuration.Files = []ConfigFile{{Path: ruleTarget, Root: ".config/agents", SHA256: hashOf([]byte(oldRule))}}
	p, err := r.buildPackage(ctx, c, run)
	if err != nil || p.Kind != KindConfig || p.Work != filepath.Join(run.Dir, "work") {
		t.Fatalf("build package: %+v %v", p, err)
	}
	if b, err := os.ReadFile(filepath.Join(p.Work, ruleTarget)); err != nil || string(b) != oldRule {
		t.Fatalf("the clone holds %q %v", b, err)
	}
}

// A landing a crash left with the change in the file is the owner's to undo with the switch off: case rollback
// restores the journaled content and rolls the package back.
func TestTheOwnerRollsBackAnInterruptedLandingWithTheSwitchOff(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	home := fakeHome(t)
	s := configShip(t, st, home)
	c := testingCase(t, st)
	reviewedConfig(t, s, st, c)
	crash := errors.New("crash")
	s.ConfigStage = func(got string) error {
		if got == "written" {
			return crash
		}
		return nil
	}
	if _, err := s.Land(ctx, c.ID); !errors.Is(err, crash) {
		t.Fatalf("land: %v", err)
	}
	s.ConfigStage, s.ConfigTargets.Enabled = nil, false
	if _, err := s.Land(ctx, c.ID); !held(err) {
		t.Fatalf("resumed with the switch off: %v", err)
	}
	if got, _ := readTarget(t, home); got != newRule {
		t.Fatalf("the interrupted landing left %q", got)
	}
	p, err := s.RollBack(ctx, c.ID, "the owner undoes it")
	if err != nil || p.State != RolledBack {
		t.Fatalf("the owner's rollback: %+v %v", p, err)
	}
	if got, mode := readTarget(t, home); got != oldRule || mode != 0o640 {
		t.Fatalf("restored %q %v", got, mode)
	}
	if got, _ := st.CaseByID(ctx, c.ID); got.Status != store.CaseTesting {
		t.Fatalf("the case: %s", got.Status)
	}
}

// A new file that picks up the directory's default ACL would widen access: the landing refuses it before the exchange.
func TestALandingRefusesASwapFileWithExtendedAttributes(t *testing.T) {
	home := fakeHome(t)
	d, err := (ConfigTargets{Home: home, Allow: []string{ruleTarget}}).open(ruleTarget, false)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	cur, err := d.state()
	if err != nil {
		t.Fatal(err)
	}
	// an attribute list on the new file stands in for an inherited ACL, which a test cannot set everywhere
	swapXattrs = func(int) (int, error) { return 6, nil }
	defer func() { swapXattrs = func(fd int) (int, error) { return unix.Flistxattr(fd, nil) } }()
	if err := d.replace(".swap", []byte(newRule), cur); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "extended attributes") {
		t.Fatalf("replace: %v", err)
	}
	if got, _ := readTarget(t, home); got != oldRule {
		t.Fatalf("the file became %q", got)
	}
	if entries, _ := os.ReadDir(filepath.Join(home, ".config/agents")); len(entries) != 1 {
		t.Fatalf("left beside it: %v", entries)
	}
}

// systemd units are never targets: anything under a unit directory, a unit's drop-in, a quadlet, and a unit file
// wherever it lies.
func TestSystemdUnitsAreNeverTargets(t *testing.T) {
	for form, name := range map[string]string{
		"a user unit directory":        ".config/systemd/user/notes.md",
		"a unit in it":                 ".config/systemd/user/momentum-loop.service",
		"a drop-in":                    ".config/systemd/user/tool.service.d/override.conf",
		"the data unit directory":      ".local/share/systemd/user/notes.md",
		"a quadlet":                    ".config/containers/systemd/app.container",
		"a unit file elsewhere":        ".config/agents/helper.timer",
		"a drop-in elsewhere":          ".config/agents/helper.socket.d/x.conf",
		"a systemd directory anywhere": ".config/agents/systemd/x.md",
	} {
		t.Run(form, func(t *testing.T) { refusedTarget(t, ConfigTargets{}, name, "a systemd unit") })
	}
}

// A landed change the owner edited since holds no later case up: the next case proposes and lands against the file as
// it is, and the first change's rollback holds since its file changed after its own landing.
func TestALaterCaseLandsOverAnEditedLandedFile(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	home := fakeHome(t)
	s := configShip(t, st, home)
	first := testingCase(t, st)
	reviewedConfig(t, s, st, first)
	if p, err := s.Land(ctx, first.ID); err != nil || p.State != Landed {
		t.Fatalf("land: %+v %v", p, err)
	}
	second := sameEvidenceCase(t, st, first)
	// while the file holds the first change and its case is deployed, the file is busy
	if _, err := s.PrepareConfig(ctx, second.ID, ruleTarget, ""); !held(err) || !strings.Contains(err.Error(), first.ID) {
		t.Fatalf("prepared over a change in effect: %v", err)
	}
	edit := "# Rules\n\nmarker-c0ffee the owner's edit\n"
	write(t, filepath.Join(home, ruleTarget), edit)
	p, err := s.PrepareConfig(ctx, second.ID, ruleTarget, "")
	if err != nil {
		t.Fatalf("prepare after the owner's edit: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(p.Work, ruleTarget)); string(b) != edit {
		t.Fatalf("the second case proposes against %q", b)
	}
	write(t, filepath.Join(p.Work, ruleTarget), edit+"marker-c0ffee new rule\n")
	write(t, filepath.Join(p.Work, "tests/test_rules.py"), ruleTest)
	if p, err = checkPatch(t, s, ctx, second.ID, "python3 tests/test_rules.py", nil); err != nil {
		t.Fatalf("check: %v", err)
	}
	if p, err = proposePatch(t, s, ctx, second.ID, configProposal); err != nil {
		t.Fatalf("propose: %v", err)
	}
	r := &Runner{Store: st, Ship: s, Dir: s.Dir}
	if p, err = s.Reviewed(ctx, second.ID, Review{Head: p.Head, Text: "Checked every changed file."}); err != nil {
		t.Fatal(err)
	}
	if p, err = r.postReview(ctx, p); err != nil || !p.Clean() {
		t.Fatalf("post: %v", err)
	}
	if p, err = s.Land(ctx, second.ID); err != nil || p.State != Landed {
		t.Fatalf("the second landing: %+v %v", p, err)
	}
	if got, _ := readTarget(t, home); got != edit+"marker-c0ffee new rule\n" {
		t.Fatalf("landed %q", got)
	}
	if _, err := s.RollBack(ctx, first.ID, "no effect"); !held(err) || !strings.Contains(err.Error(), "changed since its change landed") {
		t.Fatalf("the first rollback over the second change: %v", err)
	}
	if p, err := s.RollBack(ctx, second.ID, "no effect"); err != nil || p.State != RolledBack {
		t.Fatalf("the second rollback: %+v %v", p, err)
	}
	if got, _ := readTarget(t, home); got != edit {
		t.Fatalf("the second rollback left %q", got)
	}
}

// The owner's rollback of an interrupted landing journals its own state before it writes: a crash after that never
// lands the change again, and the next case rollback finishes it.
func TestTheOwnersRollbackOfALandingSavesItsStateFirst(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	home := fakeHome(t)
	s := configShip(t, st, home)
	c := testingCase(t, st)
	reviewedConfig(t, s, st, c)
	crash := errors.New("crash")
	s.ConfigStage = func(got string) error {
		if got == "written" {
			return crash
		}
		return nil
	}
	if _, err := s.Land(ctx, c.ID); !errors.Is(err, crash) {
		t.Fatalf("land: %v", err)
	}
	s.ConfigStage = func(got string) error {
		if got == "rollback journaled" {
			if got, _ := readTarget(t, home); got != newRule {
				t.Fatal("the rollback wrote before its record")
			}
			return crash
		}
		return nil
	}
	if _, err := s.RollBack(ctx, c.ID, "the owner undoes it"); !errors.Is(err, crash) {
		t.Fatalf("rollback: %v", err)
	}
	s.ConfigStage = nil
	if p, _, _ := s.Load(c.ID); p.State != RollingBack {
		t.Fatalf("after the crash: %s", p.State)
	}
	if _, err := s.Land(ctx, c.ID); err == nil {
		t.Fatal("the loop landed a change the owner is rolling back")
	}
	if got, _ := readTarget(t, home); got != newRule {
		t.Fatalf("the file became %q", got)
	}
	r := &Runner{Store: st, Ship: s, Dir: s.Dir, Families: []string{"failures"}}
	if _, step, _, err := r.pick(ctx, "failures", c.ID, true); err != nil || step == StepBuild {
		t.Fatalf("picked a build while the owner rolls back: %q %v", step, err)
	}
	if p, err := s.RollBack(ctx, c.ID, "the owner undoes it"); err != nil || p.State != RolledBack {
		t.Fatalf("resumed: %+v %v", p, err)
	}
	if got, mode := readTarget(t, home); got != oldRule || mode != 0o640 {
		t.Fatalf("restored %q %v", got, mode)
	}
}

// sameEvidenceCase is a second case in testing on another's evidence, so the same signal counts its problem.
func sameEvidenceCase(t *testing.T, st *store.Store, c store.Case) store.Case {
	t.Helper()
	ctx := context.Background()
	evs, err := st.CaseEvidence(ctx, c.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	f := store.Finding{Family: "failures", Signature: "failures/v1:zz-second", Title: "the thing breaks again"}
	for _, e := range evs {
		f.Evidence = append(f.Evidence, store.Evidence{Ref: e.Ref})
	}
	second, _, err := st.ObserveCase(ctx, f, "test")
	if err != nil {
		t.Fatal(err)
	}
	mech, next := "Answer is off by one", "assert Answer() == 42"
	if second, err = st.UpdateCase(ctx, second.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &next}, "test"); err != nil {
		t.Fatal(err)
	}
	return second
}

// A landed change whose case never reached deployment (its deployment step failed or was cut short) still holds its
// file: no second case prepares over it while the file holds it.
func TestALandedChangeBeforeDeploymentKeepsItsFileBusy(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	home := fakeHome(t)
	s := configShip(t, st, home)
	first := testingCase(t, st)
	p := reviewedConfig(t, s, st, first)
	// the landing's write went in and its package says landed, but the case is still in testing
	write(t, filepath.Join(home, ruleTarget), newRule)
	os.Chmod(filepath.Join(home, ruleTarget), 0o640)
	p.State, p.TargetLanded, p.Landed = Landed, hashOf([]byte(newRule)), []string{p.Head}
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	second := sameEvidenceCase(t, st, first)
	if _, err := s.PrepareConfig(ctx, second.ID, ruleTarget, ""); !held(err) || !strings.Contains(err.Error(), first.ID) {
		t.Fatalf("prepared over a landed change before its deployment: %v", err)
	}
}

// keptOwnersEdit asserts the owner's rollback of an interrupted landing over his edit: the file keeps the edit, the
// journal stays and the case log names it, the package is rolled back with its case in testing, and the file is
// free: a later case prepares against it as it is.
func keptOwnersEdit(t *testing.T, s *Ship, st *store.Store, c store.Case, home, edit string) {
	t.Helper()
	ctx := context.Background()
	p, err := s.RollBack(ctx, c.ID, "the owner undoes it")
	if err != nil || p.State != RolledBack {
		t.Fatalf("the owner's rollback over his edit: %+v %v", p, err)
	}
	if got, _ := readTarget(t, home); got != edit {
		t.Fatalf("the file holds %q, not the owner's edit", got)
	}
	if _, err := os.Stat(filepath.Join(s.pkgDir(c.ID), configJournal)); err != nil {
		t.Fatalf("the journal: %v", err)
	}
	log, err := st.CaseLog(ctx, c.ID)
	if err != nil || len(log) == 0 {
		t.Fatalf("the case log: %v", err)
	}
	if last := log[len(log)-1]; last.Action != "landing rolled back" || !strings.Contains(last.Detail, "keeps what it holds") || !strings.Contains(last.Detail, configJournal) {
		t.Fatalf("the case log's last line: %+v", last)
	}
	if got, _ := st.CaseByID(ctx, c.ID); got.Status != store.CaseTesting {
		t.Fatalf("the case: %s", got.Status)
	}
	second := sameEvidenceCase(t, st, c)
	if q, err := s.PrepareConfig(ctx, second.ID, ruleTarget, ""); err != nil || q.TargetBase.SHA256 != hashOf([]byte(edit)) {
		t.Fatalf("a later case on the file: %+v %v", q, err)
	}
	if got, _ := readTarget(t, home); got != edit {
		t.Fatalf("the later case's preparation changed the file to %q", got)
	}
}

// The owner's rollback of a landing a crash cut short, with the owner's edit displaced to the swap name (by the
// landing's exchange, or by his rollback's own), puts the edit back in the file and keeps it there: nothing of his
// is left at the swap name, the package is rolled back and the file is free.
func TestTheOwnersRollbackOfALandingPutsADisplacedEditBack(t *testing.T) {
	for _, stage := range []string{"written", "rollback journaled"} {
		t.Run(stage, func(t *testing.T) {
			st := testStore(t)
			ctx := context.Background()
			home := fakeHome(t)
			s := configShip(t, st, home)
			c := testingCase(t, st)
			reviewedConfig(t, s, st, c)
			crash := errors.New("crash")
			s.ConfigStage = func(got string) error {
				if got == "written" {
					return crash
				}
				return nil
			}
			if _, err := s.Land(ctx, c.ID); !errors.Is(err, crash) {
				t.Fatalf("land: %v", err)
			}
			edit := "marker-c0ffee the owner's edit\n"
			swap := filepath.Join(home, ".config/agents", swapName(c.ID))
			if stage == "rollback journaled" {
				// his rollback recorded itself, then its exchange put the old content in and displaced his edit, made
				// over the change, to the swap name; the crash came before the compare
				s.ConfigStage = func(got string) error {
					if got == stage {
						return crash
					}
					return nil
				}
				if _, err := s.RollBack(ctx, c.ID, "the owner undoes it"); !errors.Is(err, crash) {
					t.Fatalf("rollback: %v", err)
				}
				if p, _, _ := s.Load(c.ID); p.State != RollingBack {
					t.Fatalf("after the rollback's crash: %s", p.State)
				}
				write(t, filepath.Join(home, ruleTarget), oldRule)
			}
			s.ConfigStage = nil
			write(t, swap, edit) // the exchange displaced his edit there, and the crash came before the compare
			keptOwnersEdit(t, s, st, c, home, edit)
			if _, err := os.Lstat(swap); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("left at the swap name: %v", err)
			}
		})
	}
}

// A landing that finds the owner's rollback record of itself holds: the crash came between that record and its state.
func TestALandingHoldsOnTheOwnersRollbackRecord(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	home := fakeHome(t)
	s := configShip(t, st, home)
	c := testingCase(t, st)
	reviewedConfig(t, s, st, c)
	crash := errors.New("crash")
	s.ConfigStage = func(got string) error {
		if got == "written" {
			return crash
		}
		return nil
	}
	if _, err := s.Land(ctx, c.ID); !errors.Is(err, crash) {
		t.Fatalf("land: %v", err)
	}
	s.ConfigStage = nil
	rb := ConfigRollback{From: ConfigState{SHA256: hashOf([]byte(newRule))}, To: hashOf([]byte(oldRule)), Why: "the owner undoes it", Landing: true}
	if err := writeDurable(s.pkgDir(c.ID), configRollback, rb); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Land(ctx, c.ID); !held(err) || !strings.Contains(err.Error(), "rolling back") {
		t.Fatalf("resumed the landing: %v", err)
	}
	if p, _, _ := s.Load(c.ID); p.State != Landing {
		t.Fatalf("after the hold: %s", p.State)
	}
	if p, err := s.RollBack(ctx, c.ID, "the owner undoes it"); err != nil || p.State != RolledBack {
		t.Fatalf("the owner's rollback: %+v %v", p, err)
	}
	if got, _ := readTarget(t, home); got != oldRule {
		t.Fatalf("restored %q", got)
	}
}
