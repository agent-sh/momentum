package capacity

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// cargoTarget makes a Cargo target directory with some build output. sources, when set, are what its top-level
// dep-info file says it was built from.
func cargoTarget(t *testing.T, dir string, sources ...string) {
	t.Helper()
	write(t, filepath.Join(dir, "CACHEDIR.TAG"), cargoTag+"\n# This file is a cache directory tag created by cargo.\n")
	write(t, filepath.Join(dir, ".rustc_info.json"), "{}")
	blob(t, filepath.Join(dir, "debug", "deps", "libx.rlib"), 2<<20)
	if len(sources) > 0 {
		d := filepath.Join(dir, "debug", "app") + ":"
		for _, s := range sources {
			d += " " + s
		}
		write(t, filepath.Join(dir, "debug", "app.d"), d+"\n")
	}
}

func TestTargetsGoWhenIdleIgnoredAndOfAKnownProject(t *testing.T) {
	gitEnv(t)
	base := t.TempDir()
	projects, cache := filepath.Join(base, "projects"), filepath.Join(base, "cache")
	now := time.Now().Add(72 * time.Hour)
	// a repository that ignores its target
	app := filepath.Join(projects, "app")
	write(t, filepath.Join(app, ".gitignore"), "target/\n")
	write(t, filepath.Join(app, "src", "main.rs"), "fn main() {}\n")
	gitT(t, app, "init", "-q")
	gitT(t, app, "add", ".")
	gitT(t, app, "commit", "-qm", "init")
	cargoTarget(t, filepath.Join(app, "target"))
	// one that does not ignore it
	loose := filepath.Join(projects, "loose")
	write(t, filepath.Join(loose, "README"), "x\n")
	gitT(t, loose, "init", "-q")
	cargoTarget(t, filepath.Join(loose, "target"))
	// an excluded project's, by its remote
	ex := filepath.Join(projects, "renamed")
	write(t, filepath.Join(ex, ".gitignore"), "target/\n")
	gitT(t, ex, "init", "-q")
	gitT(t, ex, "remote", "add", "origin", "git@github.com:owner/secret.git")
	cargoTarget(t, filepath.Join(ex, "target"))
	// outside any repository: built from app's sources, or from sources that are gone
	cargoTarget(t, filepath.Join(cache, "app-target"), filepath.Join(app, "src", "main.rs"))
	cargoTarget(t, filepath.Join(cache, "orphan-target"), filepath.Join(base, "gone", "src", "lib.rs"))
	cargoTarget(t, filepath.Join(cache, "lane-target"), filepath.Join(base, "gone", "lane-x", "crates", "a", "src", "lib.rs"))
	// a build an hour ago, one a process is in, and Cargo's registry cache (no .rustc_info.json)
	cargoTarget(t, filepath.Join(cache, "busy-target"), filepath.Join(app, "src", "main.rs"))
	must(t, os.Chtimes(filepath.Join(cache, "busy-target", "debug"), now, now.Add(-time.Hour)))
	cargoTarget(t, filepath.Join(cache, "open-target"), filepath.Join(app, "src", "main.rs"))
	write(t, filepath.Join(cache, "registry", "CACHEDIR.TAG"), cargoTag+"\n# created by cargo\n")
	blob(t, filepath.Join(cache, "registry", "cache", "x.crate"), 2<<20)

	proc := procRoot(t, fakeProc{pid: 5, ppid: 1, argv: []string{"cargo"}, cwd: filepath.Join(cache, "open-target", "debug")})
	cfg := Config{Targets: TargetsConfig{Roots: []string{projects, cache}},
		Exclude: Exclude{Remotes: []string{"owner/secret"}, Names: []string{"*lane-x*"}}}.Normal()
	cfg.Live = []string{RuleTargets}
	st := openStore(t)
	res, err := Run(context.Background(), cfg, st, options(t, proc, now, nil))
	if err != nil {
		t.Fatal(err)
	}
	got := findings(res, RuleTargets)
	want := map[string]string{
		filepath.Join(app, "target"):          ActDelete,
		filepath.Join(loose, "target"):        ActReport, // git does not ignore it
		filepath.Join(ex, "target"):           ActReport, // an excluded project's remote
		filepath.Join(cache, "app-target"):    ActDelete, // built from a known repository's sources
		filepath.Join(cache, "orphan-target"): ActReport, // its project is unknown
		filepath.Join(cache, "lane-target"):   ActReport, // its sources carry an excluded name
		filepath.Join(cache, "open-target"):   ActKeep,   // a process is in it
	}
	for p, a := range want {
		if got[p].Action != a {
			t.Errorf("%s: %q (%s), want %q", p, got[p].Action, got[p].Why, a)
		}
	}
	for _, p := range []string{filepath.Join(cache, "busy-target"), filepath.Join(cache, "registry")} {
		if _, ok := got[p]; ok {
			t.Errorf("%s is not judged: built an hour ago, or not a target directory", p)
		}
	}
	for p, a := range want {
		_, err := os.Stat(p)
		if gone := os.IsNotExist(err); gone != (a == ActDelete) {
			t.Errorf("%s: gone %v, want gone %v", p, gone, a == ActDelete)
		}
	}
	if got[filepath.Join(loose, "target")].class != "tracked" || got[filepath.Join(cache, "orphan-target")].class != "unknown" ||
		got[filepath.Join(ex, "target")].class != "excluded" {
		t.Error("each report-only target says why, as its note's group")
	}
}

func TestATargetsRootThatIsASymlinkIsWalkedWhereItLeads(t *testing.T) {
	base := t.TempDir()
	real, link := filepath.Join(base, "disk", "code"), filepath.Join(base, "code")
	target := filepath.Join(real, "app", "target")
	write(t, filepath.Join(target, "CACHEDIR.TAG"), cargoTag+"\n# This file is a cache directory tag created by cargo.\n")
	write(t, filepath.Join(target, ".rustc_info.json"), "{}")
	must(t, os.Symlink(real, link))
	f := Discover([]string{link}, func(string) bool { return true }, func(string) bool { return false }, nil)
	if len(f.Targets) != 1 || f.Targets[0] != target {
		t.Errorf("a root moved to another disk and linked back is walked: %v", f.Targets)
	}
}
