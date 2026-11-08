package loop

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
	"golang.org/x/sys/unix"
)

// codeHolds are the reasons a configuration file holds its case, as the package's code declares them: every constant
// of type fileHold. A check added to the code with a reason of its own is in the list.
func codeHolds(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, decl := range f.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.CONST {
					continue
				}
				for _, spec := range gd.Specs {
					vs := spec.(*ast.ValueSpec)
					if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "fileHold" {
						continue
					}
					for _, v := range vs.Values {
						lit, ok := v.(*ast.BasicLit)
						if !ok {
							t.Fatalf("a fileHold that is no string literal: %#v", v)
						}
						s, _ := strconv.Unquote(lit.Value)
						out = append(out, s)
					}
				}
			}
		}
	}
	slices.Sort(out)
	return out
}

// A configuration file that holds its case holds it at selection, for every reason the code has: the case takes no
// slot of its family, whose other case gets its run in the same pass, and the hold is no run the stop rule counts. A
// reason only a privileged or racing fixture reaches says so and is skipped.
func TestAFileHoldTakesNoSlotOfItsFamily(t *testing.T) {
	file := func(home string) string { return filepath.Join(home, ruleTarget) }
	fixtures := map[string]func(t *testing.T, r *Runner, home string) string{
		"targets off":     func(t *testing.T, r *Runner, home string) string { r.Ship.ConfigTargets.Enabled = false; return "" },
		"refused by name": func(t *testing.T, r *Runner, home string) string { r.Ship.ConfigTargets.Allow = nil; return "" },
		"home": func(t *testing.T, r *Runner, home string) string {
			link := filepath.Join(t.TempDir(), "home")
			if err := os.Symlink(home, link); err != nil {
				t.Fatal(err)
			}
			r.Ship.ConfigTargets.Home = link
			return ""
		},
		"directory": func(t *testing.T, r *Runner, home string) string {
			dir := filepath.Join(home, ".config/agents")
			if err := os.Rename(dir, dir+"-real"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("agents-real", dir); err != nil {
				t.Fatal(err)
			}
			return ""
		},
		"missing": func(t *testing.T, r *Runner, home string) string {
			if err := os.Remove(file(home)); err != nil {
				t.Fatal(err)
			}
			return ""
		},
		"file system": func(t *testing.T, r *Runner, home string) string {
			// another file system mounted on the file's directory, holding the file
			dir := filepath.Join(home, ".config/agents")
			mustMount(t, "tmpfs", dir, "tmpfs", 0, "mode=0700")
			write(t, filepath.Join(home, ruleTarget), oldRule)
			chmod(t, filepath.Join(home, ruleTarget), 0o640)
			return ""
		},
		"link": func(t *testing.T, r *Runner, home string) string {
			real := filepath.Join(t.TempDir(), "zz-real.md")
			write(t, real, oldRule)
			if err := os.Remove(file(home)); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, file(home)); err != nil {
				t.Fatal(err)
			}
			return ""
		},
		"irregular": func(t *testing.T, r *Runner, home string) string {
			if err := os.Remove(file(home)); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(file(home), 0o700); err != nil {
				t.Fatal(err)
			}
			return ""
		},
		"replaced while read": func(*testing.T, *Runner, string) string {
			return "a file replaced between two reads of one check is a race"
		},
		"executable": func(t *testing.T, r *Runner, home string) string { chmod(t, file(home), 0o750); return "" },
		"set-id": func(t *testing.T, r *Runner, home string) string {
			chmod(t, file(home), 0o640|os.ModeSetuid)
			return ""
		},
		"owner": func(t *testing.T, r *Runner, home string) string {
			uid := targetUID
			targetUID = func() int { return os.Getuid() + 1 }
			t.Cleanup(func() { targetUID = uid })
			return ""
		},
		"hard link": func(t *testing.T, r *Runner, home string) string {
			if err := os.Link(file(home), file(home)+"-zz-twin"); err != nil {
				t.Fatal(err)
			}
			return ""
		},
		"size": func(t *testing.T, r *Runner, home string) string {
			agentWrites(t, home, strings.Repeat("marker-c0ffee line\n", MaxConfigFileBytes/19+1))
			return ""
		},
		"text": func(t *testing.T, r *Runner, home string) string { agentWrites(t, home, oldRule+"\x00\n"); return "" },
		"mount point": func(t *testing.T, r *Runner, home string) string {
			// a file of the same file system mounted on the file's name
			other := filepath.Join(filepath.Dir(home), "elsewhere.md")
			write(t, other, oldRule)
			chmod(t, other, 0o640)
			mustMount(t, other, filepath.Join(home, ruleTarget), "", unix.MS_BIND, "")
			return ""
		},
		"own file": func(t *testing.T, r *Runner, home string) string {
			// the loop's own directory, by identity alone: mounted on the file's directory, of the same file system, and
			// named nowhere under the home
			own := filepath.Join(filepath.Dir(home), "loop-rules")
			write(t, filepath.Join(own, "zz-rules.md"), oldRule)
			chmod(t, filepath.Join(own, "zz-rules.md"), 0o640)
			r.Ship.ConfigTargets.Own = append(r.Ship.ConfigTargets.Own, own)
			mustMount(t, own, filepath.Join(home, ".config/agents"), "", unix.MS_BIND, "")
			return ""
		},
		"masked": func(t *testing.T, r *Runner, home string) string {
			agentWrites(t, home, oldRule+"password=marker-7f3a\n")
			return ""
		},
	}
	mounts := map[string]bool{"file system": true, "mount point": true, "own file": true}
	var names []string
	for name := range fixtures {
		names = append(names, name)
	}
	slices.Sort(names)
	if got := codeHolds(t); !slices.Equal(got, names) {
		t.Errorf("the code's hold reasons %q, the fixtures' %q: a reason with no fixture is not covered", got, names)
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			home := fakeHome(t)
			c := testingCase(t, st)
			raw, _ := json.Marshal(map[string]any{"repo": "", "config": ruleTarget, "episode": c.Episode})
			if err := st.CaseNote(ctx, c.ID, repoAction, string(raw), "fixture"); err != nil {
				t.Fatal(err)
			}
			open := openCase(t, st, "failures", "open-zz-"+strings.ReplaceAll(name, " ", "-"), 2)
			r := runnerFor(t, st, fakeAgent(t, investigatedResult))
			r.Inputs.Home = home
			r.Ship = configShip(t, st, home)
			r.Ship.Dir = r.Dir
			if mounts[name] && os.Getenv(mountNamespaceEnv) == "" {
				runInMountNamespace(t) // the fixture mounts: it runs in a user and mount namespace of its own
				return
			}
			if skip := fixtures[name](t, r, home); skip != "" {
				t.Skip(skip)
			}
			// selection holds it, a dry pass's too, by the same check as the build's preparation (targetReady), for the
			// fixture's own reason
			if why, known := r.heldTarget(ctx, c, "", ruleTarget); why == "" || !known {
				t.Fatalf("selection does not hold the case: %q", why)
			}
			var hold *targetHoldError
			if _, _, err := r.Ship.targetReady(ctx, ruleTarget); !errors.As(err, &hold) || string(hold.hold) != name {
				t.Fatalf("the file holds for another reason than %q: %v", name, err)
			}
			done, err := r.Once(ctx, Options{Families: []string{"failures"}})
			if err != nil {
				t.Fatal(err)
			}
			ran, said := false, ""
			for _, d := range done {
				said += d.Case + " " + d.Step + " " + d.Skipped + "\n"
				ran = ran || d.Case == open.ID && d.Step == StepInvestigate && d.Run.ID != ""
				if d.Case == c.ID && d.Run.ID != "" {
					t.Errorf("the held case had a run: %+v", d)
				}
			}
			if !ran {
				t.Fatalf("the family's other case got no run while the configuration case is held:\n%s", said)
			}
		})
	}
}

// mountNamespaceEnv is set in a test process that runs in a user and mount namespace of its own (runInMountNamespace),
// where a fixture may mount.
const mountNamespaceEnv = "MOMENTUM_TEST_MOUNT_NAMESPACE"

// runInMountNamespace runs the current test again in a user and mount namespace of its own (unshare -rm, mounts private
// to it), where its fixture mounts what it needs. It skips where the machine gives no such namespace.
func runInMountNamespace(t *testing.T) {
	t.Helper()
	if out, err := exec.Command("unshare", "-rm", "true").CombinedOutput(); err != nil {
		t.Skipf("no user and mount namespace here: %v %s", err, out)
	}
	pattern := "^" + strings.ReplaceAll(regexp.QuoteMeta(t.Name()), "/", "$/^") + "$"
	cmd := exec.Command("unshare", "-rm", "--propagation", "private", os.Args[0], "-test.run="+pattern, "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), mountNamespaceEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--- PASS: "+t.Name()) {
		t.Fatalf("in its mount namespace: %v\n%s", err, out)
	}
}

// mustMount mounts, in the test's own mount namespace only.
func mustMount(t *testing.T, source, target, fstype string, flags uintptr, data string) {
	t.Helper()
	if os.Getenv(mountNamespaceEnv) == "" {
		t.Fatal("a mount outside the test's own mount namespace")
	}
	if err := unix.Mount(source, target, fstype, flags, data); err != nil {
		t.Fatalf("mount %s on %s: %v", source, target, err)
	}
	t.Cleanup(func() { unix.Unmount(target, unix.MNT_DETACH) }) // before the temporary directories go
}

func chmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// retireRow is a package a case holds when something prepares another: its kind, state, and whether it has an open
// pull request or a filed proposal.
type retireRow struct {
	name, kind, state string
	pr, filed         bool
}

// A package that has not ended is never replaced: a preparation for the same place refuses, as there is one (the
// pass's build keeps working it), and one for another repository, kind or file holds the case with a reason that reads
// nothing and names what ends the package; nothing closes its request, archives it or resolves its note. A package
// that ended (withdrawn or rolled back) gives way: the new package archives it with what it kept beside its record.
func TestAPackageThatHasNotEndedIsNeverReplaced(t *testing.T) {
	const otherTarget = ".config/agents/zz-other.md"
	const pr = "https://github.com/agent-sh/fake/pull/7"
	rows := []retireRow{
		{"repo prepared", KindRepo, Prepared, false, false}, {"repo checked", KindRepo, Checked, false, false},
		{"repo revising", KindRepo, Revising, false, false}, {"repo revising with a pull request", KindRepo, Revising, true, false},
		{"repo proposed with a pull request", KindRepo, Proposed, true, false},
		{"config prepared", KindConfig, Prepared, false, false}, {"config checked", KindConfig, Checked, false, false},
		{"config revising", KindConfig, Revising, false, false}, {"config proposed", KindConfig, Proposed, false, false},
		{"config filed", KindConfig, Proposed, false, true},
		{"repo withdrawn", KindRepo, Withdrawn, false, false}, {"repo rolled back", KindRepo, RolledBack, false, false},
		{"config withdrawn", KindConfig, Withdrawn, false, false},
	}
	triggers := []string{"owner prepare", "new episode", "new repository", "new kind or file"}
	for _, row := range rows {
		for _, trig := range triggers {
			t.Run(row.name+", "+trig, func(t *testing.T) {
				ctx := context.Background()
				st := testStore(t)
				f, g := newRepo(t, nil), newRepo(t, nil)
				s := shipWithSandbox(st, f, Sandbox{})
				s.Gate.Resolve = func(_ context.Context, dir string) (string, error) {
					if under(dir, g.dir) {
						return "agent-sh/other", nil
					}
					return "agent-sh/fake", nil
				}
				home := fakeHome(t)
				write(t, filepath.Join(home, otherTarget), oldRule)
				chmod(t, filepath.Join(home, otherTarget), 0o640)
				s.ConfigTargets = ConfigTargets{Enabled: true, Allow: []string{ruleTarget, otherTarget}, Home: home}
				c := testingCase(t, st)
				// the row's package
				var p Package
				var err error
				if row.kind == KindRepo {
					p, err = s.Prepare(ctx, c.ID, f.main)
				} else {
					p, err = s.PrepareConfig(ctx, c.ID, ruleTarget, "")
				}
				if err != nil {
					t.Fatal(err)
				}
				p.State = row.state
				if row.pr {
					p.PR, p.Head = pr, strings.Repeat("c0ffee", 7)[:40]
				}
				if row.kind == KindConfig && row.state == Proposed {
					p.Head, p.TargetNew = strings.Repeat("c0ffee", 7)[:40], hashOf([]byte(newRule))
				}
				if row.filed {
					p.Filed = time.Now()
					p.Reviews = []Review{{Round: 1, Head: p.Head, Text: "clean", Comment: "case log: review round 1", At: time.Now()}}
				}
				if err := s.save(&p); err != nil {
					t.Fatal(err)
				}
				if err := s.configNote(ctx, c, &p); err != nil {
					t.Fatal(err)
				}
				// what a checked package keeps beside its record, which an archive keeps
				write(t, filepath.Join(s.pkgDir(c.ID), "change.patch"), "a patch\n")
				write(t, filepath.Join(s.pkgDir(c.ID), "check.json"), "{}\n")
				same := false // the trigger prepares for the row's own place
				switch trig {
				case "owner prepare":
					same = row.kind == KindRepo
					_, err = s.Prepare(ctx, c.ID, f.main)
				case "new episode":
					same = true
					c = comeBack(t, st, c, "", "")
					if row.kind == KindRepo {
						_, err = s.prepare(ctx, c.ID, f.main, nil, false)
					} else {
						_, err = s.PrepareConfig(ctx, c.ID, ruleTarget, "")
					}
				case "new repository":
					_, err = s.prepare(ctx, c.ID, g.main, nil, false)
				case "new kind or file":
					if row.kind == KindRepo {
						_, err = s.PrepareConfig(ctx, c.ID, ruleTarget, "")
					} else {
						_, err = s.PrepareConfig(ctx, c.ID, otherTarget, "")
					}
				}
				got, _, _ := s.Load(c.ID)
				calls, _ := os.ReadFile(filepath.Join(f.dir, "gh-calls"))
				standing := false
				if n, ok := noteByID(t, st, configNoteID(c.ID)); ok && n.Done.IsZero() {
					standing = true
				}
				past := filepath.Join(s.pkgDir(c.ID), "past", archiveName(p))
				_, archived := os.Stat(filepath.Join(past, "package.json"))
				if !ended(p) {
					want := "must end first"
					if same {
						want = "a package already"
					}
					if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), want) {
						t.Fatalf("the trigger over a package that has not ended: %v, want a refusal naming %q", err, want)
					}
					if !same && (!held(err) || !strings.Contains(err.Error(), "ship withdraw --case "+c.ID)) {
						t.Fatalf("another place's preparation does not hold with the way the package ends: %v", err)
					}
					if !got.Created.Equal(p.Created) || got.State != row.state || got.PR != p.PR || !got.Filed.Equal(p.Filed) || archived == nil {
						t.Fatalf("the package that has not ended: %+v, archived %v", got, archived == nil)
					}
					if len(calls) > 0 {
						t.Fatalf("the preparation called gh: %s", calls)
					}
					if row.filed && !standing {
						t.Fatal("the filed proposal lost its note")
					}
					return
				}
				if err != nil {
					t.Fatalf("the trigger over a package that ended: %v", err)
				}
				if got.State != Prepared || got.Created.Equal(p.Created) {
					t.Fatalf("no new package: %+v", got)
				}
				for _, name := range []string{"package.json", "change.patch", "check.json"} {
					if _, err := os.Stat(filepath.Join(past, name)); err != nil {
						t.Fatalf("the ended package's %s is not archived: %v", name, err)
					}
				}
				for _, name := range []string{"change.patch", "check.json"} {
					if _, err := os.Stat(filepath.Join(s.pkgDir(c.ID), name)); err == nil {
						t.Fatalf("the ended package's %s stays beside the new one", name)
					}
				}
			})
		}
	}
}

// A filed note says what to do when its apply line fails because the file changed since the proposal: the change by
// hand on the file as it is, and the close with the hash the file then holds.
func TestAFiledNoteSaysWhatToDoWhenItsFileChanged(t *testing.T) {
	r, c, home, _ := filedConfigCase(t)
	n, _ := noteByID(t, r.Store, configNoteID(c.ID))
	if !strings.Contains(n.Detail, "If the line fails because the file changed") ||
		!strings.Contains(n.Detail, `--fix 'config:~/`+ruleTarget+`@'"$(sha256sum `+ShellQuote(filepath.Join(home, ruleTarget))+` | cut -c1-64)"`) {
		t.Fatalf("the filed note: %s", n.Detail)
	}
}

// A file the run's configuration copy left out is known only once the inputs are built, after selection: its build is
// refused, a failed run the stop rule counts, not a hold that takes its family's slot every pass.
func TestACopyThatLeftTheFileOutIsACountedRefusal(t *testing.T) {
	ctx := context.Background()
	r, c, _, _ := proposedConfigCase(t)
	if _, err := r.Ship.Reviewed(ctx, c.ID, Review{Head: loadPackage(t, r.Ship, c.ID).Head, Medium: 1, Text: "a finding"}); err != nil {
		t.Fatal(err)
	}
	r.reviews(ctx, c.ID)
	r.pass.manifest.Configuration.Files = nil
	run := store.LoopRun{ID: "fixture-build-2", Case: c.ID, Dir: filepath.Join(r.Dir, "runs", "fixture-build-2"), CaseEpisode: c.Episode}
	if _, err := r.buildPackage(ctx, c, run); !errors.Is(err, ErrRefused) || held(err) || !strings.Contains(err.Error(), "left out") {
		t.Fatalf("the build of a file the copy left out: %v (held %v)", err, held(err))
	}
}

func loadPackage(t *testing.T, s *Ship, id string) Package {
	t.Helper()
	p, ok, err := s.Load(id)
	if err != nil || !ok {
		t.Fatalf("load %s: %v %v", id, ok, err)
	}
	return p
}
