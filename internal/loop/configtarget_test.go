package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
		ConfigTargets: ConfigTargets{Enabled: true, Allow: []string{".config/agents"}, Home: home}}
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

// targetRoots are allow entries for every root a target may lie in.
var targetRoots = []string{".claude/mods", ".claude/hooks", ".claude/settings.json", ".codex", ".hermes", ".pi/agent", ".config/opencode",
	".grok", ".config/agents", ".config/systemd/user"}

// A target is a file the configuration copy reads, under the stricter target denylist, that allow names: an empty
// allow admits nothing, and an entry cannot widen the copy's list.
func TestConfigTargetsAreTheCopysFilesUnderAStricterDenylist(t *testing.T) {
	ct := ConfigTargets{Allow: targetRoots}
	for _, ok := range []string{ruleTarget, "~/" + ruleTarget, "/ro/config/" + ruleTarget, ".config/agents/procedures/x.md", ".claude/settings.json",
		".claude/hooks/guard.sh", ".claude/mods/m/index.js", ".codex/config.toml", ".codex/rules/r.rules", ".hermes/skills/s/SKILL.md",
		".pi/agent/settings.json", ".config/opencode/opencode.json", ".grok/config.toml", ".config/systemd/user/tool.service"} {
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
		".claude/mods/node_modules/a.js",
		// no target in .local/bin (the credential tools and the loop's programs are there) or the loop's own configuration
		".local/bin/tool", ".local/bin/cred-get", ".local/bin/waitfor", ".config/momentum/sources.json",
		// the target's own: credentials, keys, brokers and auth of any spelling, other key stores, journals, the loop's own
		".claude/hooks/cred-exec.sh", ".claude/hooks/cred_set.py", ".claude/hooks/keys.json", ".claude/hooks/claude-broker.sh",
		".claude/hooks/pre-auth.sh", ".claude/hooks/my-secrets.md", ".claude/hooks/tokens.md", ".claude/hooks/git", ".claude/hooks/waitfor.sh",
		".config/systemd/user/momentum-loop.service", ".claude/hooks/.netrc", ".claude/hooks/.npmrc", ".claude/hooks/known_hosts",
		".claude/hooks/x.sqlite3", ".claude/hooks/x.db-wal", ".claude/hooks/key.gpg",
		// a drop-in directory's file, by a directory entry
		".config/systemd/user/tool.service.d/override.conf",
	} {
		if _, err := ct.admit(bad); !errors.Is(err, ErrRefused) {
			t.Errorf("%q admitted: %v", bad, err)
		}
	}
	for name, why := range map[string]string{".local/bin/tool": "a program on PATH", ".config/momentum/sources.json": "the loop's own configuration"} {
		if _, err := ct.admit(name); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), why) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := (ConfigTargets{}).admit(ruleTarget); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "names no file") {
		t.Fatalf("an empty allow: %v", err)
	}
	dropIn := ".config/systemd/user/tool.service.d/override.conf"
	if _, err := (ConfigTargets{Allow: []string{dropIn}}).admit(dropIn); err != nil {
		t.Fatalf("a drop-in named exactly: %v", err)
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
	if _, err := (ConfigTargets{Allow: []string{".config/agents/RULES.md"}}).admit(".config/agents/OTHER.md"); !errors.Is(err, ErrRefused) {
		t.Fatalf("an exact entry admitted another file: %v", err)
	}
	// an allow entry cannot widen the copy's list: it refuses every target instead
	for _, wide := range []string{".ssh/**", "/etc/**", ".bashrc", ".local/bin", ".config/momentum", ".claude/hooks/cred-get", ".config/agents/*.md", ".config/agents/R?LES.md", ".config/agents/[R]ULES.md"} {
		if _, err := (ConfigTargets{Allow: []string{wide, ".config/agents"}}).admit(ruleTarget); !errors.Is(err, ErrRefused) {
			t.Fatalf("allow %s: %v", wide, err)
		}
	}
}

// The target is read under the real home without following a link, in any component, and within the copy's limits.
func TestConfigTargetsAreReadWithoutLinksWithinLimits(t *testing.T) {
	home := fakeHome(t)
	ct := ConfigTargets{Home: home, Allow: []string{".config/agents", ".claude/hooks"}}
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
	write(t, filepath.Join(home, ".config/agents/SHARED.md"), "marker-c0ffee linked twice\n")
	if err := os.Link(filepath.Join(home, ".config/agents/SHARED.md"), filepath.Join(home, ".config/agents/HARD.md")); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{".config/agents/LINK.md", ".config/agents/procedures/x.md", ".config/agents/DIR.md", ".config/agents/BIG.md",
		".config/agents/BAD.md", ".config/agents/NUL.md", ".config/agents/HARD.md", ".config/agents/SHARED.md", ".claude/hooks/x.sh"} {
		if _, err := read(bad); !errors.Is(err, ErrRefused) {
			t.Errorf("%s read: %v", bad, err)
		}
	}
	// a file of another owner is no target
	targetUID = func() int { return os.Getuid() + 1 }
	_, err := read(ruleTarget)
	targetUID = os.Getuid
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("another owner's file: %v", err)
	}
	// a home reached through a link is no home
	linked := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(home, linked); err != nil {
		t.Fatal(err)
	}
	if _, err := (ConfigTargets{Home: linked, Allow: []string{".config/agents"}}).open(ruleTarget); !errors.Is(err, ErrRefused) {
		t.Fatalf("linked home: %v", err)
	}
}

// The write is a compare-and-swap: a file that is not what the step read stays as it is, and nothing is left beside it.
func TestConfigWritesOnlyOverTheContentExpected(t *testing.T) {
	home := fakeHome(t)
	d, err := (ConfigTargets{Home: home, Allow: []string{".config/agents"}}).open(ruleTarget)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	read, err := d.state()
	if err != nil {
		t.Fatal(err)
	}
	other := *read.bare()
	other.SHA256 = hashOf([]byte("marker-c0ffee other\n"))
	if err := d.replace(".swap", []byte(newRule), 0o640, other); !errors.Is(err, ErrRefused) {
		t.Fatalf("replace over other content: %v", err)
	}
	if err := d.replace(".swap", []byte(newRule), 0o640, ConfigState{Absent: true}); !errors.Is(err, ErrRefused) {
		t.Fatalf("a new file: %v", err)
	}
	if got, mode := readTarget(t, home); got != oldRule || mode != 0o640 {
		t.Fatalf("the file became %q %v", got, mode)
	}
	if entries, _ := os.ReadDir(filepath.Join(home, ".config/agents")); len(entries) != 1 {
		t.Fatalf("left beside it: %v", entries)
	}
	// a swap file that does not hold what the step put there stays, and that is no refusal
	write(t, filepath.Join(home, ".config/agents/.swap"), "marker-c0ffee someone else's\n")
	if err := d.unlinkIf(".swap", hashOf([]byte(newRule))); err == nil || errors.Is(err, ErrRefused) {
		t.Fatalf("unlinked another's swap file: %v", err)
	}
	if err := d.settle(".swap", ConfigState{SHA256: hashOf([]byte(newRule))}, ConfigState{SHA256: hashOf([]byte(oldRule))}); !errors.Is(err, ErrRefused) {
		t.Fatalf("settled over another's swap file: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(home, ".config/agents/.swap")); err != nil || string(b) != "marker-c0ffee someone else's\n" {
		t.Fatalf("the swap file became %q %v", b, err)
	}
	os.Remove(filepath.Join(home, ".config/agents/.swap"))
	if err := d.replace(".swap", []byte(newRule), 0o640, *read.bare()); err != nil {
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
	// the loop resumes its rollback only while it may write; the owner's own rollback restores the file whatever the switch
	s.Live = false
	if _, err := s.rollBack(ctx, c.ID, "resume rollback", false, false); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "waits") {
		t.Fatalf("the loop resumed while not live: %v", err)
	}
	s.Live, s.ConfigTargets.Enabled = true, false
	if _, err := s.rollBack(ctx, c.ID, "resume rollback", false, false); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "config_targets") {
		t.Fatalf("the loop resumed while the switch is off: %v", err)
	}
	if got, _ := readTarget(t, home); got != drift {
		t.Fatalf("a held rollback wrote %q", got)
	}
	s.Live = false
	// the journaled drift is what the resumed rollback may overwrite, with no second --force
	p, err := s.RollBack(ctx, c.ID, "the owner resumes")
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
	r.pass = &passInputs{}
	c := testingCase(t, st)
	raw, _ := json.Marshal(map[string]any{"repo": "", "config": ruleTarget, "episode": c.Episode})
	if err := st.CaseNote(ctx, c.ID, repoAction, string(raw), "fixture"); err != nil {
		t.Fatal(err)
	}
	if _, step, _, err := r.pick(ctx, "failures", c.ID, true); err != nil || step != StepBuild {
		t.Fatalf("pick: %q %v", step, err)
	}
	run := store.LoopRun{ID: "fixture-build", Case: c.ID, Dir: filepath.Join(r.Dir, "runs", "fixture-build"), CaseEpisode: c.Episode}
	// a target the run's copy left out is not what the run saw
	if _, err := r.buildPackage(ctx, c, run); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "left out") {
		t.Fatalf("a target outside the copy: %v", err)
	}
	r.pass.manifest.Configuration.Files = []ConfigFile{{Path: ruleTarget, Root: ".config/agents"}}
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

// A configuration package edits a file that exists, its tests lie under tests/, and two cases never change one file
// at once: a second preparation waits while the first package is not rolled back or kept.
func TestAConfigurationTargetExistsHasOneChangeAndTestsUnderTests(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	home := fakeHome(t)
	s := configShip(t, st, home)
	c := testingCase(t, st)
	if _, err := s.PrepareConfig(ctx, c.ID, ".config/agents/NEW.md"); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "not there") {
		t.Fatalf("a new file: %v", err)
	}
	p, err := s.PrepareConfig(ctx, c.ID, ruleTarget)
	if err != nil {
		t.Fatal(err)
	}
	other := testingCase2(t, st)
	if _, err := s.PrepareConfig(ctx, other.ID, ruleTarget); !errors.Is(err, ErrRefused) || !held(err) || !strings.Contains(err.Error(), c.ID) {
		t.Fatalf("a second change to the same file: %v", err)
	}
	write(t, filepath.Join(p.Work, ruleTarget), newRule)
	write(t, filepath.Join(p.Work, "check_test.py"), ruleTest)
	if _, err := checkPatch(t, s, ctx, c.ID, "python3 check_test.py", []string{"check_test.py"}); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "under tests/") {
		t.Fatalf("a test outside tests/: %v", err)
	}
	if _, err := checkPatch(t, s, ctx, c.ID, "python3 check_test.py", nil); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "no test file") {
		t.Fatalf("a test outside tests/ by its name: %v", err)
	}
	// once the first package is done, the file is free
	p, _, _ = s.Load(c.ID)
	p.State = RolledBack
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareConfig(ctx, other.ID, ruleTarget); err != nil {
		t.Fatalf("after the first is done: %v", err)
	}
}

// When the exchange back after a mismatch fails, the landing is no refusal: its journal and its Landing state stay
// for the owner, and nothing marks the change landed.
func TestAFailedExchangeBackKeepsTheLandingsJournal(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	home := fakeHome(t)
	s := configShip(t, st, home)
	c := testingCase(t, st)
	reviewedConfig(t, s, st, c)
	calls := 0
	exchange = func(olddirfd int, oldpath string, newdirfd int, newpath string, flags uint) error {
		calls++
		if calls == 1 {
			// the owner edits the file just before the swap
			write(t, filepath.Join(home, ruleTarget), "marker-c0ffee the owner's edit\n")
			return unix.Renameat2(olddirfd, oldpath, newdirfd, newpath, flags)
		}
		return unix.EIO
	}
	defer func() { exchange = unix.Renameat2 }()
	_, err := s.Land(ctx, c.ID)
	if err == nil || errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "exchange back failed") {
		t.Fatalf("land: %v", err)
	}
	if p, _, _ := s.Load(c.ID); p.State != Landing || p.TargetLanded != "" {
		t.Fatalf("after a failed exchange back: %s", p.State)
	}
	if _, err := os.Stat(filepath.Join(s.pkgDir(c.ID), configJournal)); err != nil {
		t.Fatalf("the journal: %v", err)
	}
}

// A configuration change with no measured effect is rolled back by the measure step only while configuration targets
// are on: with the switch off the rollback is held and the file keeps the change.
func TestTheMeasureStepHoldsAConfigurationRollbackWhileTheSwitchIsOff(t *testing.T) {
	m := measuredCase(t)
	home := fakeHome(t)
	write(t, filepath.Join(home, ruleTarget), newRule)
	p, _, _ := m.r.Ship.Load(m.c.ID)
	p.Kind, p.Target, p.TargetLanded, p.Repo, p.Dir, p.PR = KindConfig, ruleTarget, hashOf([]byte(newRule)), "", "", ""
	p.TargetBase = &ConfigState{SHA256: hashOf([]byte(oldRule)), Mode: 0o640}
	if err := m.r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	j := ConfigJournal{Target: ruleTarget, Old: ConfigState{SHA256: hashOf([]byte(oldRule)), Mode: 0o640, Content: []byte(oldRule)},
		New: ConfigState{SHA256: hashOf([]byte(newRule)), Mode: 0o640}, Head: p.Landed[0]}
	if err := writeDurable(m.r.Ship.pkgDir(m.c.ID), configJournal, j); err != nil {
		t.Fatal(err)
	}
	m.r.Ship.Live, m.r.Ship.ConfigTargets = true, ConfigTargets{Enabled: false, Allow: []string{".config/agents"}, Home: home}
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
	if got, _ := readTarget(t, home); got != oldRule {
		t.Fatalf("the rollback left %q", got)
	}
}

// A chmod by the owner after review is a change of the file: the landing does not undo it, it builds again on the file
// as it is now.
func TestALandingNeverRevertsTheOwnersChmod(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	home := fakeHome(t)
	s := configShip(t, st, home)
	c := testingCase(t, st)
	reviewedConfig(t, s, st, c)
	if err := os.Chmod(filepath.Join(home, ruleTarget), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := s.Land(ctx, c.ID)
	if !errors.Is(err, ErrRefused) || p.State != Revising || !p.RefreshBase {
		t.Fatalf("land after a chmod: %+v %v", p.State, err)
	}
	if got, mode := readTarget(t, home); got != oldRule || mode != 0o600 {
		t.Fatalf("the file became %q %v", got, mode)
	}
}

// The owner's rollback and the in-effect check read a landed change's own target after the owner narrowed allow; the
// loop's own rollback holds then.
func TestANarrowedAllowLeavesTheOwnersRollback(t *testing.T) {
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
	s.ConfigTargets.Allow = nil
	if _, why, err := s.InEffect(ctx, &p); err != nil || why != "" {
		t.Fatalf("in effect after allow narrowed: %q %v", why, err)
	}
	if hold := s.mayApply(p); !strings.Contains(hold, "not admitted") {
		t.Fatalf("the loop's hold: %q", hold)
	}
	if _, err := s.rollBackLocked(ctx, c.ID, "no effect"); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "waits") {
		t.Fatalf("the loop rolled back after allow narrowed: %v", err)
	}
	if p, err = s.RollBack(ctx, c.ID, "the owner undoes it"); err != nil || p.State != RolledBack {
		t.Fatalf("the owner's rollback: %+v %v", p.State, err)
	}
	if got, _ := readTarget(t, home); got != oldRule {
		t.Fatalf("restored %q", got)
	}
}

// A file whose case was closed by hand frees its target; one being prepared holds it.
func TestAClosedCaseFreesItsTargetAndPreparationsAreSerialized(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	home := fakeHome(t)
	s := configShip(t, st, home)
	c := testingCase(t, st)
	if _, err := s.PrepareConfig(ctx, c.ID, ruleTarget); err != nil {
		t.Fatal(err)
	}
	other := testingCase2(t, st)
	unlock, err := s.lockTarget(ruleTarget)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareConfig(ctx, other.ID, ruleTarget); !held(err) || !strings.Contains(err.Error(), "another preparation") {
		t.Fatalf("a concurrent preparation: %v", err)
	}
	unlock()
	if _, err := s.PrepareConfig(ctx, other.ID, ruleTarget); !held(err) {
		t.Fatalf("a live case's package: %v", err)
	}
	if _, err := st.CloseCase(ctx, c.ID, store.NotWorthIt, "", "marker-c0ffee", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareConfig(ctx, other.ID, ruleTarget); err != nil {
		t.Fatalf("after the case closed by hand: %v", err)
	}
}

// A landing whose exchange fails for want of support, or whose file went away, holds as proposed and spends no
// review round; a stuck landing the owner abandons returns to its build.
func TestALandingHoldsOrIsAbandonedWithoutAReviewRound(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	home := fakeHome(t)
	s := configShip(t, st, home)
	c := testingCase(t, st)
	reviewedConfig(t, s, st, c)
	exchange = func(int, string, int, string, uint) error { return unix.EINVAL }
	p, err := s.Land(ctx, c.ID)
	exchange = unix.Renameat2
	if !held(err) || p.State != Proposed || p.RefreshBase || !p.Clean() {
		t.Fatalf("no exchange support: %+v %v", p.State, err)
	}
	// a crash after the journal, then the owner's edit: the landing waits for him, and he abandons it
	s.ConfigStage = func(stage string) error {
		if stage == "journaled" {
			return errors.New("crash")
		}
		return nil
	}
	s.Land(ctx, c.ID)
	s.ConfigStage = nil
	write(t, filepath.Join(home, ruleTarget), "marker-c0ffee the owner's edit\n")
	if _, err := s.Land(ctx, c.ID); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "abandon") {
		t.Fatalf("stuck: %v", err)
	}
	if p, err = s.AbandonLanding(ctx, c.ID, "marker-c0ffee keep my edit"); err != nil || p.State != Revising || !p.RefreshBase {
		t.Fatalf("abandon: %+v %v", p.State, err)
	}
	if got, _ := readTarget(t, home); got != "marker-c0ffee the owner's edit\n" {
		t.Fatalf("abandon wrote %q", got)
	}
}

// A forced rollback of a landed change whose file was deleted is refused before its journal.
func TestAForcedRollbackOfAGoneFileIsRefused(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	c2 := testingCase(t, st)
	home2 := fakeHome(t)
	s2 := configShip(t, st, home2)
	reviewedConfig(t, s2, st, c2)
	if _, err := s2.Land(ctx, c2.ID); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(home2, ruleTarget))
	if _, err := s2.RollBackForced(ctx, c2.ID, "gone"); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("forced over a gone file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s2.pkgDir(c2.ID), configRollback)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journaled before refusing: %v", err)
	}
}

// A swap file left by an interrupted exchange that holds someone else's edit, while the target holds the change, is
// exchanged back: the edit is the file's again, and the landing goes back to its build.
func TestSettleExchangesBackADisplacedEdit(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	home := fakeHome(t)
	s := configShip(t, st, home)
	c := testingCase(t, st)
	reviewedConfig(t, s, st, c)
	s.ConfigStage = func(stage string) error {
		if stage == "written" {
			return errors.New("crash")
		}
		return nil
	}
	s.Land(ctx, c.ID)
	s.ConfigStage = nil
	edit := "marker-c0ffee an edit the exchange displaced\n"
	write(t, filepath.Join(home, ".config/agents", swapName(c.ID)), edit)
	p, err := s.Land(ctx, c.ID)
	if !errors.Is(err, ErrRefused) || p.State != Revising || !p.RefreshBase {
		t.Fatalf("resume over a displaced edit: %+v %v", p.State, err)
	}
	if got, _ := readTarget(t, home); got != edit {
		t.Fatalf("the file holds %q", got)
	}
	if _, err := os.Stat(filepath.Join(home, ".config/agents", swapName(c.ID))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the swap file stays: %v", err)
	}
}

// The loop's resumed rollback waits while it may not write: at a pass's selection and in the measure step.
func TestAResumedConfigurationRollbackWaitsForTheSwitch(t *testing.T) {
	m := measuredCase(t)
	home := fakeHome(t)
	write(t, filepath.Join(home, ruleTarget), newRule)
	p, _, _ := m.r.Ship.Load(m.c.ID)
	p.Kind, p.Target, p.TargetLanded, p.Repo, p.Dir, p.PR, p.State = KindConfig, ruleTarget, hashOf([]byte(newRule)), "", "", "", RollingBack
	if err := m.r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	m.r.Ship.Live, m.r.Ship.ConfigTargets = true, ConfigTargets{Enabled: false, Allow: []string{".config/agents"}, Home: home}
	m.r.Families = []string{"failures"}
	if got := m.measure(t, Options{}); got.Status != "held" || !strings.Contains(got.Why, "waits") {
		t.Fatalf("the measure step: %+v", got)
	}
	if _, _, why, err := m.r.pick(context.Background(), "failures", m.c.ID, false); err != nil || !strings.Contains(why, m.c.ID+"'s rollback waits") {
		t.Fatalf("the pick: %q %v", why, err)
	}
	if got, _ := readTarget(t, home); got != newRule {
		t.Fatalf("a held rollback wrote %q", got)
	}
}

// A target with extended attributes (an ACL, a label) is refused: a rewrite would drop them.
func TestATargetWithExtendedAttributesIsRefused(t *testing.T) {
	home := fakeHome(t)
	if err := syscall.Setxattr(filepath.Join(home, ruleTarget), "user.marker", []byte("c0ffee"), 0); err != nil {
		t.Skipf("no user extended attributes here: %v", err)
	}
	d, err := (ConfigTargets{Home: home, Allow: []string{".config/agents"}}).open(ruleTarget)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.state(); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "extended attributes") {
		t.Fatalf("a target with an attribute: %v", err)
	}
}

// The rewritten file keeps the group the file had, not the caller's primary one.
func TestALandingKeepsTheFilesGroup(t *testing.T) {
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	other := -1
	for _, g := range groups {
		if g != os.Getgid() {
			other = g
			break
		}
	}
	if other < 0 {
		t.Skip("the caller has no second group")
	}
	home := fakeHome(t)
	if err := os.Chown(filepath.Join(home, ruleTarget), -1, other); err != nil {
		t.Skip(err)
	}
	d, err := (ConfigTargets{Home: home, Allow: []string{".config/agents"}}).open(ruleTarget)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	cur, err := d.state()
	if err != nil {
		t.Fatal(err)
	}
	if err := d.replace(".swap", []byte(newRule), cur.Mode, cur); err != nil {
		t.Fatal(err)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(filepath.Join(home, ruleTarget), &st); err != nil || int(st.Gid) != other {
		t.Fatalf("the group became %d (%v), not %d", st.Gid, err, other)
	}
}
