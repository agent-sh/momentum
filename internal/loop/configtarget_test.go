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
	ruleTarget = ".config/agents/RULES.md"
	oldRule    = "# Rules\n\nmarker-c0ffee old rule\n"
	newRule    = "# Rules\n\nmarker-c0ffee new rule\n"
	ruleTest   = "import sys\ntext = open('.config/agents/RULES.md').read()\nsys.exit(0 if 'marker-c0ffee new rule' in text else 1)\n"
)

// fakeHome is a home under the test's own directory with one rules file of the agent-sync root.
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
		ConfigTargets: ConfigTargets{Enabled: true, Home: home}}
}

var configProposal = Proposal{Title: "Rules say the new rule", Body: "The old rule caused the failure.", Rollback: "restore the journaled file",
	Lesson: "Rules say the new rule.", Signal: goodSignal, Window: 7 * 24 * time.Hour}

// proposedConfig prepares, checks and proposes the rules change.
func proposedConfig(t *testing.T, s *Ship, c store.Case) Package {
	t.Helper()
	ctx := context.Background()
	p, err := s.PrepareConfig(ctx, c.ID, "/ro/config/"+ruleTarget)
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
	st, err := os.Stat(filepath.Join(home, ruleTarget))
	if err != nil {
		t.Fatal(err)
	}
	return string(b), st.Mode().Perm()
}

// A target is a file the configuration copy reads, under the stricter target denylist, and Allow only narrows it.
func TestConfigTargetsAreTheCopysFilesUnderAStricterDenylist(t *testing.T) {
	ct := ConfigTargets{}
	for _, ok := range []string{ruleTarget, "~/" + ruleTarget, "/ro/config/" + ruleTarget, ".config/agents/procedures/x.md", ".claude/settings.json",
		".claude/hooks/guard.sh", ".claude/mods/m/index.js", ".codex/config.toml", ".codex/rules/r.rules", ".hermes/skills/s/SKILL.md",
		".pi/agent/settings.json", ".config/opencode/opencode.json", ".grok/config.toml", ".config/systemd/user/tool.service", ".local/bin/tool"} {
		if _, err := ct.admit(ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, bad := range []string{
		// outside every root, and the copy's own exclusions
		".bashrc", ".ssh/config", ".aws/credentials", ".claude.json", ".claude/projects/x.md", ".codex/sessions/s.md", ".codex/history.md",
		".pi/agent/models.json", ".hermes/state.md", "/etc/passwd", "../x", ".config/agents/../x", ".config/agents/./x.md", "",
		// the copy's denylist: credentials, keys, environment files, databases, logs, auth
		".config/agents/bedrock.env", ".claude/hooks/.env", ".codex/auth.json", ".claude/hooks/keyring.py", ".claude/hooks/token.sh",
		".claude/hooks/secret.sh", ".claude/hooks/id_rsa", ".claude/hooks/x.pem", ".claude/hooks/oauth.js", ".claude/hooks/vault.sh",
		".config/agents/procedures/x.sqlite", ".claude/hooks/x.db", ".claude/hooks/events.jsonl", ".claude/hooks/run.log",
		".claude/mods/node_modules/a.js", ".local/bin/sub/tool",
		// the target's own: other key stores, database journals and the loop's own configuration, units and programs
		".config/momentum/sources.json", ".config/systemd/user/momentum-loop.service", ".local/bin/momentum", ".claude/hooks/.netrc",
		".claude/hooks/.npmrc", ".claude/hooks/known_hosts", ".claude/hooks/x.sqlite3", ".claude/hooks/x.db-wal", ".claude/hooks/key.gpg",
	} {
		if _, err := ct.admit(bad); !errors.Is(err, ErrRefused) {
			t.Errorf("%q admitted: %v", bad, err)
		}
	}
	narrow := ConfigTargets{Allow: []string{"~/.config/agents/**", ".claude/settings.json"}}
	if _, err := narrow.admit(ruleTarget); err != nil {
		t.Fatal(err)
	}
	if _, err := narrow.admit(".claude/settings.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := narrow.admit(".codex/config.toml"); !errors.Is(err, ErrRefused) {
		t.Fatalf("outside allow: %v", err)
	}
	if _, err := (ConfigTargets{Allow: []string{".config/agents/*.md"}}).admit(".config/agents/procedures/x.md"); !errors.Is(err, ErrRefused) {
		t.Fatalf("a pattern matched below its directory: %v", err)
	}
	// an allow entry cannot widen the copy's list: it refuses every target instead
	for _, wide := range []string{".ssh/**", "/etc/**", ".bashrc"} {
		if _, err := (ConfigTargets{Allow: []string{wide}}).admit(ruleTarget); !errors.Is(err, ErrRefused) {
			t.Fatalf("allow %s: %v", wide, err)
		}
	}
}

// The target is read under the real home without following a link, in any component, and within the copy's limits.
func TestConfigTargetsAreReadWithoutLinksWithinLimits(t *testing.T) {
	home := fakeHome(t)
	ct := ConfigTargets{Home: home}
	read := func(name string) (ConfigState, error) {
		d, err := ct.open(name)
		if err != nil {
			return ConfigState{}, err
		}
		defer d.Close()
		return d.state()
	}
	if got, err := read(ruleTarget); err != nil || got.SHA256 != hashOf([]byte(oldRule)) || got.Mode != 0o640 || string(got.Content) != oldRule {
		t.Fatalf("%+v %v", got, err)
	}
	if got, err := read(".config/agents/NEW.md"); err != nil || !got.Absent {
		t.Fatalf("a new file: %+v %v", got, err)
	}
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	write(t, filepath.Join(elsewhere, "x.md"), "marker-c0ffee elsewhere\n")
	if err := os.Symlink(filepath.Join(elsewhere, "x.md"), filepath.Join(home, ".config/agents/LINK.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(home, ".config/agents/procedures")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".config/agents/DIR.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(home, ".config/agents/BIG.md"), strings.Repeat("x", MaxConfigFileBytes+1))
	write(t, filepath.Join(home, ".config/agents/BAD.md"), "marker-c0ffee \xff\n")
	write(t, filepath.Join(home, ".config/agents/NUL.md"), strings.Repeat("x", 9<<10)+"\x00")
	write(t, filepath.Join(home, ".local/bin/tool"), "marker-c0ffee no shebang\n")
	for _, bad := range []string{".config/agents/LINK.md", ".config/agents/procedures/x.md", ".config/agents/DIR.md", ".config/agents/BIG.md",
		".config/agents/BAD.md", ".config/agents/NUL.md", ".local/bin/tool", ".claude/hooks/x.sh"} {
		if _, err := read(bad); !errors.Is(err, ErrRefused) {
			t.Errorf("%s read: %v", bad, err)
		}
	}
	// a home reached through a link is no home
	linked := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(home, linked); err != nil {
		t.Fatal(err)
	}
	if _, err := (ConfigTargets{Home: linked}).open(ruleTarget); !errors.Is(err, ErrRefused) {
		t.Fatalf("linked home: %v", err)
	}
}

// The write is a compare-and-swap: a file that is not what the step read stays as it is, and nothing is left beside it.
func TestConfigWritesOnlyOverTheContentExpected(t *testing.T) {
	home := fakeHome(t)
	d, err := (ConfigTargets{Home: home}).open(ruleTarget)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	other := ConfigState{SHA256: hashOf([]byte("marker-c0ffee other\n"))}
	if err := d.replace(".swap", []byte(newRule), 0o640, other); !errors.Is(err, ErrRefused) {
		t.Fatalf("replace over other content: %v", err)
	}
	if err := d.replace(".swap", []byte(newRule), 0o640, ConfigState{Absent: true}); !errors.Is(err, ErrRefused) {
		t.Fatalf("a new file over an existing one: %v", err)
	}
	if err := d.remove(".swap", other); !errors.Is(err, ErrRefused) {
		t.Fatalf("remove other content: %v", err)
	}
	if got, mode := readTarget(t, home); got != oldRule || mode != 0o640 {
		t.Fatalf("the file became %q %v", got, mode)
	}
	if entries, _ := os.ReadDir(filepath.Join(home, ".config/agents")); len(entries) != 1 {
		t.Fatalf("left beside it: %v", entries)
	}
	if err := d.replace(".swap", []byte(newRule), 0o640, ConfigState{SHA256: hashOf([]byte(oldRule))}); err != nil {
		t.Fatal(err)
	}
	if got, mode := readTarget(t, home); got != newRule || mode != 0o640 {
		t.Fatalf("the file became %q %v", got, mode)
	}
}

// A configuration change goes through the repository package's gates: a check that fails before and passes after,
// a clean review posted (to the case's log), and a landing only while the switch is on and the loop is live. The
// previous content is journaled before the write, the change is in effect while the file holds what landed, and the
// rollback restores the file exactly.
func TestAConfigurationChangeShipsThroughTheSameGates(t *testing.T) {
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
	if log, _ := st.CaseLog(ctx, c.ID); !strings.Contains(log[len(log)-1].Detail, "Checked every changed file.") {
		t.Fatalf("the review is not in the case's log: %+v", log[len(log)-1])
	}
	if _, err := s.Land(ctx, c.ID); !errors.Is(err, ErrRefused) {
		t.Fatalf("landed on a medium: %v", err)
	}
	// the revision: a second check and proposal, reviewed clean
	write(t, filepath.Join(p.Work, ruleTarget), newRule+"\nmarker-c0ffee kept\n")
	if _, err := checkPatch(t, s, ctx, c.ID, "python3 tests/test_rules.py", nil); err != nil {
		t.Fatal(err)
	}
	if p, err = proposePatch(t, s, ctx, c.ID, configProposal); err != nil {
		t.Fatal(err)
	}
	if p, err = s.Reviewed(ctx, c.ID, Review{Head: p.Head, Text: "Checked every changed file."}); err != nil || p.Clean() {
		t.Fatalf("an unposted review: %+v %v", p, err)
	}
	if _, err := s.Land(ctx, c.ID); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "posted") {
		t.Fatalf("landed on an unposted review: %v", err)
	}
	if p, err = r.postReview(ctx, p); err != nil || !p.Clean() {
		t.Fatal(err)
	}
	s.ConfigTargets.Enabled = false
	if _, err := s.Land(ctx, c.ID); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "config_targets") {
		t.Fatalf("landed while the switch is off: %v", err)
	}
	if done := r.reviews(ctx, ""); len(done) != 1 || !strings.Contains(done[0].Skipped, "config_targets") {
		t.Fatalf("the pass with the switch off: %+v", done)
	}
	s.ConfigTargets.Enabled, s.Live = true, false
	if _, err := s.Land(ctx, c.ID); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "not live") {
		t.Fatalf("landed while not live: %v", err)
	}
	if got, mode := readTarget(t, home); got != oldRule || mode != 0o640 {
		t.Fatalf("a refused landing wrote the file: %q %v", got, mode)
	}
	if _, err := os.Stat(filepath.Join(s.pkgDir(c.ID), configJournal)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused landing journaled: %v", err)
	}
	s.Live = true
	if p, err = s.Land(ctx, c.ID); err != nil || p.State != Landed || p.TargetLanded != hashOf([]byte(newRule+"\nmarker-c0ffee kept\n")) {
		t.Fatalf("land: %+v %v", p, err)
	}
	if got, mode := readTarget(t, home); got != newRule+"\nmarker-c0ffee kept\n" || mode != 0o640 {
		t.Fatalf("landed %q %v", got, mode)
	}
	if got, _ := st.CaseByID(ctx, c.ID); got.Status != store.CaseDeployed {
		t.Fatalf("the case after landing: %s", got.Status)
	}
	info, err := os.Stat(filepath.Join(s.pkgDir(c.ID), configJournal))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("journal: %v %v", info, err)
	}
	var j ConfigJournal
	if ok, err := readRecord(s.pkgDir(c.ID), configJournal, &j); !ok || err != nil || string(j.Old.Content) != oldRule || j.Old.SHA256 != hashOf([]byte(oldRule)) || j.Old.Mode != 0o640 {
		t.Fatalf("journal: %+v %v", j, err)
	}
	if _, why, err := s.InEffect(ctx, &p); err != nil || why != "" {
		t.Fatalf("in effect: %q %v", why, err)
	}
	write(t, filepath.Join(home, ruleTarget), oldRule)
	if _, why, err := s.InEffect(ctx, &p); err != nil || !strings.Contains(why, "does not hold") {
		t.Fatalf("out of effect: %q %v", why, err)
	}
	write(t, filepath.Join(home, ruleTarget), newRule+"\nmarker-c0ffee kept\n")
	if err := os.Chmod(filepath.Join(home, ruleTarget), 0o640); err != nil {
		t.Fatal(err)
	}
	if p, err = s.RollBack(ctx, c.ID, "it did nothing"); err != nil || p.State != RolledBack {
		t.Fatalf("rollback: %+v %v", p, err)
	}
	if got, mode := readTarget(t, home); got != oldRule || mode != 0o640 {
		t.Fatalf("the rollback left %q %v", got, mode)
	}
	if got, _ := st.CaseByID(ctx, c.ID); got.Status != store.CaseTesting {
		t.Fatalf("the case after the rollback: %s", got.Status)
	}
	if p, err = s.RollBack(ctx, c.ID, "again"); err != nil || p.State != RolledBack {
		t.Fatalf("a second rollback is its receipt: %+v %v", p, err)
	}
	if p, err = s.PrepareConfig(ctx, c.ID, ruleTarget); err != nil || p.State != Prepared {
		t.Fatalf("prepared again: %+v %v", p, err)
	}
}

// A file whose text the scrubber would mask is not a target: the run's copy would not be the file the host patches.
func TestAConfigurationTargetMustReadThroughTheScrubberUnchanged(t *testing.T) {
	st := testStore(t)
	home := fakeHome(t)
	write(t, filepath.Join(home, ruleTarget), "marker-c0ffee password=marker-7f3a\n")
	s := configShip(t, st, home)
	c := testingCase(t, st)
	if _, err := s.PrepareConfig(context.Background(), c.ID, ruleTarget); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "scrubber") {
		t.Fatalf("a masked target: %v", err)
	}
}

// A configuration change edits its target and adds tests, nothing else, keeps the mode and never deletes the file.
func TestAConfigurationChangeTouchesOnlyItsTarget(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	home := fakeHome(t)
	s := configShip(t, st, home)
	c := testingCase(t, st)
	p, err := s.PrepareConfig(ctx, c.ID, ruleTarget)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Work, "tests/test_rules.py"), ruleTest)
	write(t, filepath.Join(p.Work, ".config/agents/OTHER.md"), "marker-c0ffee other\n")
	write(t, filepath.Join(p.Work, ruleTarget), newRule)
	if _, err := checkPatch(t, s, ctx, c.ID, "python3 tests/test_rules.py", nil); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "neither the target") {
		t.Fatalf("a second file: %v", err)
	}
	os.Remove(filepath.Join(p.Work, ".config/agents/OTHER.md"))
	if err := os.Chmod(filepath.Join(p.Work, ruleTarget), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := checkPatch(t, s, ctx, c.ID, "python3 tests/test_rules.py", nil); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("a mode change: %v", err)
	}
	os.Remove(filepath.Join(p.Work, ruleTarget))
	if _, err := checkPatch(t, s, ctx, c.ID, "python3 tests/test_rules.py", nil); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "deletes") {
		t.Fatalf("a deletion: %v", err)
	}
	write(t, filepath.Join(p.Work, ruleTarget), "marker-c0ffee \x00 new rule\n")
	if _, err := checkPatch(t, s, ctx, c.ID, "python3 tests/test_rules.py", nil); !errors.Is(err, ErrRefused) {
		t.Fatalf("a NUL in the new content: %v", err)
	}
}

// The landing journals the previous content before it writes, and a landing a crash interrupted is finished from that
// journal: written again when the file still holds the old content, finished when it holds the new, and refused,
// with the owner's edit kept, when it holds neither.
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
				at = "journaled"
			}
			s.ConfigStage = func(got string) error {
				if got == at {
					if got == "journaled" {
						// the journal is on disk, with the old content, before the file is touched
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
				return nil
			}
			if _, err := s.Land(ctx, c.ID); !errors.Is(err, crash) {
				t.Fatalf("land: %v", err)
			}
			s.ConfigStage = nil
			if p, _, _ := s.Load(c.ID); p.State != Landing {
				t.Fatalf("after the crash: %s", p.State)
			}
			if stage == "written" {
				// the swap's scratch name left beside the file is cleared too
				write(t, filepath.Join(home, ".config/agents", swapName(c.ID)), oldRule)
			}
			if stage == "drifted" {
				write(t, filepath.Join(home, ruleTarget), "marker-c0ffee the owner's edit\n")
				if _, err := s.Land(ctx, c.ID); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "neither") {
					t.Fatalf("resumed over the owner's edit: %v", err)
				}
				if got, _ := readTarget(t, home); got != "marker-c0ffee the owner's edit\n" {
					t.Fatalf("the owner's edit became %q", got)
				}
				return
			}
			s.Live = false
			if _, err := s.Land(ctx, c.ID); !errors.Is(err, ErrRefused) {
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

// A rollback refuses a file changed since its change landed, unless the owner forces it, and then journals what it
// overwrites. One a crash interrupted is finished from its journal, once.
func TestAConfigurationRollbackRefusesADriftedFileAndRecovers(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	home := fakeHome(t)
	s := configShip(t, st, home)
	c := testingCase(t, st)
	reviewedConfig(t, s, st, c)
	if _, err := s.Land(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	drift := "marker-c0ffee the owner's later edit\n"
	write(t, filepath.Join(home, ruleTarget), drift)
	if _, err := s.RollBack(ctx, c.ID, "no effect"); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("rolled back over a drifted file: %v", err)
	}
	if got, _ := readTarget(t, home); got != drift {
		t.Fatalf("the refused rollback wrote %q", got)
	}
	crash := errors.New("crash")
	s.ConfigStage = func(stage string) error {
		if stage == "rollback journaled" {
			return crash
		}
		return nil
	}
	if _, err := s.RollBackForced(ctx, c.ID, "the owner wants it back"); !errors.Is(err, crash) {
		t.Fatalf("forced: %v", err)
	}
	s.ConfigStage = nil
	var rb ConfigRollback
	if ok, err := readRecord(s.pkgDir(c.ID), configRollback, &rb); !ok || err != nil || string(rb.From.Content) != drift || !rb.Force {
		t.Fatalf("the rollback journal: %+v %v", rb, err)
	}
	s.Live = false
	if _, err := s.RollBack(ctx, c.ID, "resume rollback"); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "loop.live") {
		t.Fatalf("resumed while not live: %v", err)
	}
	s.Live = true
	// the journaled drift is what the resumed rollback may overwrite, with no second --force
	p, err := s.RollBack(ctx, c.ID, "resume rollback")
	if err != nil || p.State != RolledBack {
		t.Fatalf("resumed: %+v %v", p, err)
	}
	if got, mode := readTarget(t, home); got != oldRule || mode != 0o640 {
		t.Fatalf("restored %q %v", got, mode)
	}
	if got, _ := st.CaseByID(ctx, c.ID); got.Status != store.CaseTesting {
		t.Fatalf("the case: %s", got.Status)
	}
}

// A file restored by hand before the rollback makes the rollback its receipt, and the measure step holds an
// automatic rollback while the switch is off.
func TestAConfigurationRollbackIsIdempotent(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	home := fakeHome(t)
	s := configShip(t, st, home)
	c := testingCase(t, st)
	reviewedConfig(t, s, st, c)
	p, err := s.Land(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if why := s.mayApply(p); why != "" {
		t.Fatal(why)
	}
	s.ConfigTargets.Enabled = false
	if why := s.mayApply(p); !strings.Contains(why, "config_targets") {
		t.Fatalf("the hold: %q", why)
	}
	s.ConfigTargets.Enabled = true
	write(t, filepath.Join(home, ruleTarget), oldRule)
	if p, err = s.RollBack(ctx, c.ID, "restored by hand"); err != nil || p.State != RolledBack {
		t.Fatalf("rollback of a restored file: %+v %v", p, err)
	}
	if got, _ := readTarget(t, home); got != oldRule {
		t.Fatalf("%q", got)
	}
}

// An investigation names a configuration file as its fix's target; a report naming a repository too names neither.
func TestAnInvestigationNamesAConfigurationTarget(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	for i, tc := range []struct{ report, repo, config string }{
		{`{"mechanism":"one cause","fix":"one fix","summary":"marker-c0ffee","config":"/ro/config/.config/agents/RULES.md"}`, "", ruleTarget},
		{`{"mechanism":"one cause","fix":"one fix","summary":"marker-c0ffee","config":"~/.claude/settings.json","repo":"main"}`, "", ""},
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

// A testing case whose investigation named a configuration file builds a configuration package, with the switch off
// too (no host write): its run clone holds the target as the host reads it, mounted in the run's storage.
func TestABuildForAConfigurationCaseMountsItsTargetsClone(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	home := fakeHome(t)
	r := localRunner(t, st)
	r.Ship = configShip(t, st, home)
	r.Ship.Dir, r.Ship.ConfigTargets.Enabled = r.Dir, false
	c := testingCase(t, st)
	raw, _ := json.Marshal(map[string]any{"repo": "", "config": ruleTarget, "episode": c.Episode})
	if err := st.CaseNote(ctx, c.ID, repoAction, string(raw), "fixture"); err != nil {
		t.Fatal(err)
	}
	if _, step, _, err := r.pick(ctx, "failures", c.ID, true); err != nil || step != StepBuild {
		t.Fatalf("pick: %q %v", step, err)
	}
	run := store.LoopRun{ID: "fixture-build", Case: c.ID, Dir: filepath.Join(r.Dir, "runs", "fixture-build"), CaseEpisode: c.Episode}
	p, err := r.buildPackage(ctx, c, run)
	if err != nil || p.Kind != KindConfig || p.Work != filepath.Join(run.Dir, "work") {
		t.Fatalf("build package: %+v %v", p, err)
	}
	if b, err := os.ReadFile(filepath.Join(p.Work, ruleTarget)); err != nil || string(b) != oldRule {
		t.Fatalf("the clone holds %q %v", b, err)
	}
	if entries, _ := os.ReadDir(p.Work); len(entries) != 2 { // .config and .git
		t.Fatalf("the clone holds more than its target: %v", entries)
	}
}
