package capacity

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// uvEntries makes entries in a cache: each a directory with a file of 8 KiB in it (lib/x.so), so it holds blocks on
// disk. One in environments-v2, or one whose name starts with env, is an environment, with its pyvenv.cfg and its
// bin/python a link to an interpreter outside the cache, as uv makes them.
func uvEntries(t *testing.T, cache string, names ...string) {
	t.Helper()
	interp := filepath.Join(t.TempDir(), "python3.12")
	write(t, interp, "")
	for _, n := range names {
		blob(t, filepath.Join(cache, n, "lib", "x.so"), 8192)
		if strings.HasPrefix(n, "environments-v2/") || strings.HasPrefix(filepath.Base(n), "env") {
			write(t, filepath.Join(cache, n, "pyvenv.cfg"), "home = "+filepath.Dir(interp)+"\nversion_info = 3.12.13\n")
			must(t, os.MkdirAll(filepath.Join(cache, n, "bin"), 0o755))
			must(t, os.Symlink(interp, filepath.Join(cache, n, "bin", "python")))
		}
	}
}

// left fails the test for each of kept that is gone, and each of gone that stays.
func left(t *testing.T, cache string, kept, gone []string) {
	t.Helper()
	for _, n := range kept {
		if _, err := os.Lstat(filepath.Join(cache, n)); err != nil {
			t.Errorf("%s was deleted, and a process uses it: %v", n, err)
		}
	}
	for _, n := range gone {
		if _, err := os.Lstat(filepath.Join(cache, n)); err == nil {
			t.Errorf("%s stays, and nothing that runs uses it", n)
		}
	}
}

// around reports whether the rule pruned around running environments: it pruned, with no prune of uv's.
func (f *uvLockFixture) around(g Finding) bool { return g.Done && !f.pruned(true) && !f.pruned(false) }

// letGo fails the test when the lock in the cache's lock file's place cannot be taken alone: capacity kept it.
func letGo(t *testing.T, lock string) {
	t.Helper()
	fh, err := os.Open(lock)
	must(t, err)
	defer fh.Close()
	if err := syscall.Flock(int(fh.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Errorf("capacity lets the cache's lock go: %v", err)
	}
}

// byPATH is a command a uvx started from a cached environment, which only its PATH names.
func byPATH(env string) func(cache, lock string) []fakeProc {
	return fromCache(func(cache string) fakeProc {
		return fakeProc{argv: []string{"srv"}, env: started(filepath.Join(cache, env, "bin"))}
	})
}

func TestAUVPruneAroundRunningEnvironmentsKeepsWhatTheyUse(t *testing.T) {
	for _, c := range []struct {
		name  string
		procs func(cache, lock string) []fakeProc
		kept  string
	}{
		{"a tool run from the cache, by its program", fromCache(func(cache string) fakeProc {
			return fakeProc{argv: []string{"python", "srv"}, exe: filepath.Join(cache, "archive-v0", "envx", "bin", "python3")}
		}), "archive-v0/envx"},
		{"a tool run from the cache, by the program it was started as", fromCache(func(cache string) fakeProc {
			return fakeProc{argv: []string{filepath.Join(cache, "archive-v0", "envx", "bin", "python"), "srv"}}
		}), "archive-v0/envx"},
		{"a tool's command, by its PATH", byPATH("archive-v0/envx"), "archive-v0/envx"},
		{"a script's command, by VIRTUAL_ENV", fromCache(func(cache string) fakeProc {
			return fakeProc{argv: []string{"python", "s.py"}, env: []string{"VIRTUAL_ENV=" + filepath.Join(cache, "environments-v2", "s-1")}}
		}), "environments-v2/s-1"},
		{"a script whose environment is in the cache, by a mapped file", func(cache, lock string) []fakeProc {
			return append(outside(cache, lock), fakeProc{pid: 8, ppid: 1, argv: []string{"uv", "run", "--script", "s.py"},
				fds: []string{lock}, env: uvEnv}, fakeProc{pid: 9, ppid: 8, argv: []string{"python", "s.py"},
				maps: []string{filepath.Join(cache, "environments-v2", "s-1", "lib", "x.so")}, env: ran("/opt/s/bin")})
		}, "environments-v2/s-1"},
		{"a command whose PYTHONPATH names a wheel in the cache", fromCache(func(cache string) fakeProc {
			return fakeProc{argv: []string{"python", "srv"}, env: append(started("/opt/srv/bin"),
				"PYTHONPATH=/opt/lib:"+filepath.Join(cache, "archive-v0", "wheel"))}
		}), "archive-v0/wheel"},
		{"a PATH that names a directory in another of its buckets", func(cache, lock string) []fakeProc {
			path := "PATH=/usr/bin:" + filepath.Join(cache, "tools-v0", "x", "libexec") // uv's own PATH, which its command's ends with
			return append(outside(cache, lock), fakeProc{pid: 8, ppid: 1, argv: []string{"uv", "tool", "uvx", "srv"}, fds: []string{lock},
				env: []string{path}}, fakeProc{pid: 9, ppid: 8, argv: []string{"srv"}, env: []string{"PATH=/opt/srv/bin:" + path[5:]}})
		}, "tools-v0/x"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newUVLock(t, c.procs, 4, 6, 8)
			all := []string{"archive-v0/envx", "archive-v0/wheel", "archive-v0/stale", "environments-v2/s-1", "environments-v2/s-2",
				"tools-v0/x", "builds-v0/.tmpA"}
			uvEntries(t, f.cache, all...)
			g := f.run(t, options(t, f.proc, time.Now(), nil))
			if !f.around(g) || !strings.Contains(g.Detail, "so the prune goes around what they use") || !strings.Contains(g.Detail, "removed") {
				t.Fatalf("the prune runs around what the processes use: %s %q %q %v", g.Action, g.Why, g.Detail, f.ran)
			}
			// builds-v0 and another bucket stay for uv's own prune
			kept := []string{c.kept, "tools-v0/x", "builds-v0/.tmpA"}
			var gone []string
			for _, n := range all {
				if !slices.Contains(kept, n) {
					gone = append(gone, n)
				}
			}
			left(t, f.cache, kept, gone)
			letGo(t, f.lock)
		})
	}
}

func TestAnEnvironmentRunsOnWhenTheWheelsItWasMadeFromGo(t *testing.T) {
	// uv's default link mode hardlinks an environment's files from the unpacked wheels (it clones them where the
	// filesystem can, and copies across filesystems), so the wheels can go while a process imports from it
	f := newUVLock(t, fromCache(func(cache string) fakeProc {
		env := filepath.Join(cache, "archive-v0", "envtool")
		return fakeProc{argv: []string{filepath.Join(env, "bin", "python"), filepath.Join(env, "bin", "srv")},
			env: started(filepath.Join(env, "bin"))}
	}), 4, 6, 8)
	wheel := filepath.Join(f.cache, "archive-v0", "wheel-pkg")
	write(t, filepath.Join(wheel, "pkg", "__init__.py"), "VALUE = 42\n")
	env := filepath.Join(f.cache, "archive-v0", "envtool")
	write(t, filepath.Join(env, "pyvenv.cfg"), "home = /opt/python/bin\n")
	must(t, os.MkdirAll(filepath.Join(env, "bin"), 0o755))
	must(t, os.Symlink("/opt/python/bin/python3", filepath.Join(env, "bin", "python")))
	must(t, os.Symlink("lib", filepath.Join(env, "lib64")))
	site := filepath.Join(env, "lib", "python3.12", "site-packages")
	must(t, os.MkdirAll(filepath.Join(site, "pkg"), 0o755))
	must(t, os.Link(filepath.Join(wheel, "pkg", "__init__.py"), filepath.Join(site, "pkg", "__init__.py")))
	write(t, filepath.Join(site, "_virtualenv.pth"), "import _virtualenv\n")
	uvEntries(t, f.cache, "archive-v0/envold") // a cached environment no process runs from
	shard := filepath.Join(f.cache, "environments-v2", "0f1aa4ba3ab7ece7")
	must(t, os.MkdirAll(shard, 0o755))
	must(t, os.Symlink("../../archive-v0/envtool", filepath.Join(shard, "936ba4511c4f2f11")))
	must(t, os.Symlink("../../archive-v0/envold", filepath.Join(shard, "4816122a621079f0")))
	pointer := filepath.Join(f.cache, "wheels-v6", "pypi", "pkg", "1.0-py3-none-any")
	must(t, os.MkdirAll(filepath.Dir(pointer), 0o755))
	must(t, os.Symlink("../../../archive-v0/wheel-pkg", pointer))
	g := f.run(t, options(t, f.proc, time.Now(), nil))
	if !f.around(g) || !strings.Contains(g.Detail, "1 process runs from it, so the prune goes around what they use: it keeps 1 environment") {
		t.Fatalf("the prune runs around the environment a process runs from: %s %q %q %v", g.Action, g.Why, g.Detail, f.ran)
	}
	left(t, f.cache, []string{"archive-v0/envtool", "environments-v2/0f1aa4ba3ab7ece7/936ba4511c4f2f11"},
		[]string{"archive-v0/wheel-pkg", "archive-v0/envold", "environments-v2/0f1aa4ba3ab7ece7/4816122a621079f0"})
	if b, err := os.ReadFile(filepath.Join(site, "pkg", "__init__.py")); err != nil || string(b) != "VALUE = 42\n" {
		t.Errorf("the environment's files stay whole: %q %v", b, err)
	}
	if _, err := os.Lstat(pointer); err != nil {
		t.Errorf("a wheel's pointer stays for uv's own prune (uv takes one whose archive is gone as a miss): %v", err)
	}
	if exists(filepath.Join(f.cache, uvTrash)) {
		t.Error("what the prune moved aside is deleted once the lock is gone")
	}
}

func TestAnEnvironmentWhosePackagesLinkIntoTheCacheKeepsWhatTheyLinkTo(t *testing.T) {
	// uv's symlink link mode makes a package's directories and links its files to the unpacked wheel's
	f := newUVLock(t, byPATH("archive-v0/envlinked"), 4, 6, 8)
	uvEntries(t, f.cache, "archive-v0/envlinked", "archive-v0/wheel-linked", "archive-v0/wheel-other")
	pkg := filepath.Join(f.cache, "archive-v0", "envlinked", "lib", "python3.12", "site-packages", "pkg")
	must(t, os.MkdirAll(pkg, 0o755))
	must(t, os.Symlink(filepath.Join(f.cache, "archive-v0", "wheel-linked", "lib", "x.so"), filepath.Join(pkg, "x.so")))
	g := f.run(t, options(t, f.proc, time.Now(), nil))
	if !f.around(g) {
		t.Fatalf("the prune runs around the environment and what it links to: %s %q %v", g.Action, g.Why, f.ran)
	}
	left(t, f.cache, []string{"archive-v0/envlinked", "archive-v0/wheel-linked"}, []string{"archive-v0/wheel-other"})
}

func TestAnOverlayKeepsTheLayerAndTheEnvironmentItNames(t *testing.T) {
	// uv run --with runs its command in an overlay in builds-v0 whose .pth adds the layer (a cached environment) and
	// the environment beneath (here a script's). The command set its title over the environment that named the
	// overlay, so only the overlay says what it imports from.
	zeroed := []string{strings.Repeat("\x00", 512)}
	f := newUVLock(t, func(cache, lock string) []fakeProc {
		return append(outside(cache, lock),
			fakeProc{pid: 8, ppid: 1, argv: []string{"uv", "run", "--with", "six", "--script", "s.py"}, fds: []string{lock}, env: uvEnv},
			fakeProc{pid: 9, ppid: 8, argv: []string{"srv: master"}, exe: "/usr/bin/python3.12", env: zeroed})
	}, 4, 6, 8)
	uvEntries(t, f.cache, "archive-v0/envlayer", "environments-v2/s-1", "archive-v0/envstale", "environments-v2/s-2")
	overlay := filepath.Join(f.cache, "builds-v0", ".tmpOv1")
	layer := filepath.Join(f.cache, "archive-v0", "envlayer", "lib", "python3.12", "site-packages")
	base := filepath.Join(f.cache, "environments-v2", "s-1", "lib", "python3.12", "site-packages")
	write(t, filepath.Join(overlay, "lib", "python3.12", "site-packages", "_uv_ephemeral_overlay.pth"),
		fmt.Sprintf(`import site; site.addsitedir(__import__("os").fsdecode(b"%s")); site.addsitedir(__import__("os").fsdecode(b"%s"))`+"\n",
			layer, base))
	write(t, filepath.Join(overlay, "pyvenv.cfg"), "home = /usr/bin\nextends-environment = "+filepath.Join(f.cache, "environments-v2", "s-1")+"\n")
	g := f.run(t, options(t, f.proc, time.Now(), nil))
	if !f.around(g) {
		t.Fatalf("the prune runs around the overlay and what it names: %s %q %v", g.Action, g.Why, f.ran)
	}
	left(t, f.cache, []string{"builds-v0/.tmpOv1", "archive-v0/envlayer", "environments-v2/s-1"},
		[]string{"archive-v0/envstale", "environments-v2/s-2"})
}

func TestAnOverlayAKilledUVLeftKeepsWhatItNames(t *testing.T) {
	// a uv run --with was killed: its titled command runs on from the overlay, and holds no lock and names nothing.
	// uv's prune would delete builds-v0; the prune goes around it.
	zeroed := []string{strings.Repeat("\x00", 512)}
	f := newUVLock(t, func(cache, lock string) []fakeProc {
		return []fakeProc{{pid: 9, ppid: 1, argv: []string{"srv: master"}, exe: "/usr/bin/python3.12",
			maps: []string{"/proj/.venv/lib/python3.12/site-packages/setproctitle/_setproctitle.so"}, env: zeroed}}
	})
	uvEntries(t, f.cache, "builds-v0/.tmpOv1", "archive-v0/envlayer", "archive-v0/stale")
	write(t, filepath.Join(f.cache, "builds-v0", ".tmpOv1", "lib", "python3.12", "site-packages", "_uv_ephemeral_overlay.pth"),
		`import site; site.addsitedir(__import__("os").fsdecode(b"`+filepath.Join(f.cache, "archive-v0", "envlayer", "lib")+`"))`+"\n")
	g := f.run(t, options(t, f.proc, time.Now(), nil))
	if !f.around(g) || !strings.Contains(g.Detail, "builds-v0 holds an overlay a command whose uv was killed may run in") {
		t.Fatalf("an overlay a killed uv left keeps uv's prune off it and what it names: %s %q %q %v", g.Action, g.Why, g.Detail, f.ran)
	}
	left(t, f.cache, []string{"builds-v0/.tmpOv1", "archive-v0/envlayer"}, []string{"archive-v0/stale"})
	letGo(t, f.lock)
}

func TestAnOverlayKnownByItsPyvenvCfgKeepsUVsPruneOff(t *testing.T) {
	f := newUVLock(t, func(cache, lock string) []fakeProc { return []fakeProc{{pid: 4, ppid: 1, argv: []string{"bash"}}} })
	uvEntries(t, f.cache, "builds-v0/.tmpOv2", "environments-v2/s-1", "archive-v0/stale")
	write(t, filepath.Join(f.cache, "builds-v0", ".tmpOv2", "pyvenv.cfg"), "home = /usr/bin\nextends-environment = "+
		filepath.Join(f.cache, "environments-v2", "s-1")+"\n")
	g := f.run(t, options(t, f.proc, time.Now(), nil))
	if !f.around(g) {
		t.Fatalf("an overlay its pyvenv.cfg names as one keeps uv's prune off it: %s %q %v", g.Action, g.Why, f.ran)
	}
	left(t, f.cache, []string{"builds-v0/.tmpOv2", "environments-v2/s-1"}, []string{"archive-v0/stale"})
}

func TestWhatAnEnvironmentNamesInTheCacheIsKeptWithIt(t *testing.T) {
	f := newUVLock(t, byPATH("archive-v0/envmain"), 4, 6, 8)
	uvEntries(t, f.cache, "archive-v0/envmain", "archive-v0/bypth", "archive-v0/byrelpth", "archive-v0/byfinder",
		"archive-v0/byhome", "archive-v0/byshebang", "archive-v0/stale")
	env := filepath.Join(f.cache, "archive-v0", "envmain")
	site := filepath.Join(env, "lib", "python3.12", "site-packages")
	write(t, filepath.Join(site, "extra.pth"), "# a comment\n"+filepath.Join(f.cache, "archive-v0", "bypth", "lib")+
		"\n../../../../byrelpth/lib\nimport os\n")
	write(t, filepath.Join(site, "__editable___pkg_1_0_finder.py"),
		"MAPPING: dict[str, str] = {'pkg': '"+filepath.Join(f.cache, "archive-v0", "byfinder", "lib")+"'}\n")
	write(t, filepath.Join(env, "pyvenv.cfg"), "home = "+filepath.Join(f.cache, "archive-v0", "byhome", "bin")+"\n")
	write(t, filepath.Join(env, "bin", "tool"), "#!"+filepath.Join(f.cache, "archive-v0", "byshebang", "bin", "python")+"\n")
	// a path outside the cache that is a link into it, as a project's .venv that is a centralized environment
	uvEntries(t, f.cache, "archive-v0/bylink")
	venv := filepath.Join(t.TempDir(), ".venv")
	must(t, os.Symlink(filepath.Join(f.cache, "archive-v0", "bylink"), venv))
	write(t, filepath.Join(site, "base.pth"), filepath.Join(venv, "lib")+"\n")
	g := f.run(t, options(t, f.proc, time.Now(), nil))
	if !f.around(g) {
		t.Fatalf("the prune runs around the environment and what it names: %s %q %v", g.Action, g.Why, f.ran)
	}
	left(t, f.cache, []string{"archive-v0/envmain", "archive-v0/bypth", "archive-v0/byrelpth", "archive-v0/byfinder",
		"archive-v0/byhome", "archive-v0/byshebang", "archive-v0/bylink"}, []string{"archive-v0/stale"})
}

func TestAPlaceThroughALinkToACachedEnvironmentKeepsOnlyWhatItLeadsTo(t *testing.T) {
	// the command was started as the program a link of environments-v2 leads to, a link to an interpreter outside the
	// cache: the directories the place goes through say which environment it is
	f := newUVLock(t, func(cache, lock string) []fakeProc {
		cmd := fromCache(func(cache string) fakeProc {
			return fakeProc{argv: []string{filepath.Join(cache, "environments-v2", "1f34641d6828a9ac", "a", "bin", "python"), "srv"}}
		})(cache, lock)
		// and one through a link to an environment that is gone, which keeps nothing
		return append(cmd, fakeProc{pid: 11, ppid: 1, argv: []string{filepath.Join(cache, "environments-v2", "1f34641d6828a9ac",
			"gone", "bin", "python"), "old"}})
	}, 4, 6, 8)
	uvEntries(t, f.cache, "archive-v0/envused", "archive-v0/envother")
	shard := filepath.Join(f.cache, "environments-v2", "1f34641d6828a9ac")
	must(t, os.MkdirAll(shard, 0o755))
	must(t, os.Symlink("../../archive-v0/envused", filepath.Join(shard, "a")))
	must(t, os.Symlink("../../archive-v0/envother", filepath.Join(shard, "b")))
	must(t, os.Symlink("../../archive-v0/envgone", filepath.Join(shard, "gone")))
	g := f.run(t, options(t, f.proc, time.Now(), nil))
	if !f.around(g) {
		t.Fatalf("the prune runs around the environment the link leads to: %s %q %v", g.Action, g.Why, f.ran)
	}
	left(t, f.cache, []string{"archive-v0/envused", "environments-v2/1f34641d6828a9ac/a"},
		[]string{"archive-v0/envother", "environments-v2/1f34641d6828a9ac/b", "environments-v2/1f34641d6828a9ac/gone"})
}

func TestAnInterpreterStartedThroughALinkIntoTheCacheKeepsItsEnvironment(t *testing.T) {
	// a job started a project's server as its .venv's python, and the .venv is a link to a centralized environment:
	// no uv gave it a PATH or VIRTUAL_ENV, and nothing it maps is in the cache
	base := t.TempDir()
	f := newUVLock(t, func(cache, lock string) []fakeProc {
		return []fakeProc{{pid: 9, ppid: 1, argv: []string{filepath.Join(base, "proj", ".venv", "bin", "python"), "serve.py"},
			env: []string{"PATH=/usr/bin"}}}
	})
	uvEntries(t, f.cache, "environments-v2/proj-cpython3.12-1", "environments-v2/proj-cpython3.12-2")
	must(t, os.MkdirAll(filepath.Join(base, "proj"), 0o755))
	must(t, os.Symlink(filepath.Join(f.cache, "environments-v2", "proj-cpython3.12-1"), filepath.Join(base, "proj", ".venv")))
	g := f.run(t, options(t, f.proc, time.Now(), nil))
	if !f.around(g) {
		t.Fatalf("an environment's interpreter, started by a name outside the cache, keeps the environment: %s %q %v", g.Action, g.Why, f.ran)
	}
	left(t, f.cache, []string{"environments-v2/proj-cpython3.12-1"}, []string{"environments-v2/proj-cpython3.12-2"})
}

func TestAfterAPruneAroundTheHoldersLockKeepsUVsOwnCleanWaiting(t *testing.T) {
	// a uvx server holds uv's lock, shared, for its life; after the prune around it, a uv cache clean must wait for it
	// again, as it waited before
	f := newUVLock(t, byPATH("archive-v0/envx"), 4, 6, 8)
	uvEntries(t, f.cache, "archive-v0/envx", "archive-v0/stale")
	server, err := os.OpenFile(f.lock, os.O_RDWR, 0)
	must(t, err)
	defer server.Close()
	must(t, syscall.Flock(int(server.Fd()), syscall.LOCK_SH))
	g := f.run(t, options(t, f.proc, time.Now(), nil))
	if !f.around(g) {
		t.Fatalf("the prune runs around the server: %s %q %v", g.Action, g.Why, f.ran)
	}
	clean, err := os.OpenFile(f.lock, os.O_RDWR, 0)
	must(t, err)
	defer clean.Close()
	if err := syscall.Flock(int(clean.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		t.Error("a clean after the prune takes uv's lock at once, while the server that holds it runs from the cache")
	}
	if exists(f.lock+uvAsideSuffix) || exists(f.lock+".capacity") {
		t.Error("no file of capacity's is left beside the lock")
	}
}

func TestACleanThatCameWhileCapacityHeldTheLockIsWaitedOut(t *testing.T) {
	defer func(d time.Duration) { uvCleanWait = d }(uvCleanWait)
	uvCleanWait = 300 * time.Millisecond
	f := newUVLock(t, byPATH("archive-v0/envx"), 4, 6, 8)
	uvEntries(t, f.cache, "archive-v0/envx", "archive-v0/stale")
	o := options(t, f.proc, time.Now(), nil)
	o.afterUVLock = func() { // a uv cache clean opened capacity's file, and waits for it alone
		var st syscall.Stat_t
		must(t, syscall.Stat(f.lock, &st))
		b, err := os.ReadFile(filepath.Join(f.proc, "locks"))
		must(t, err)
		write(t, filepath.Join(f.proc, "locks"), string(b)+fmt.Sprintf("9: -> FLOCK  ADVISORY  WRITE 31 %02x:%02x:%d 0 EOF\n",
			f.id.major, f.id.minor, st.Ino))
	}
	start := time.Now()
	g := f.run(t, o)
	if !f.around(g) || !strings.Contains(g.Detail, "waited for it past 300ms") || time.Since(start) < uvCleanWait {
		t.Fatalf("capacity keeps its lock while a clean that came to it waits, and says so: %s %q %q after %s", g.Action, g.Why,
			g.Detail, time.Since(start))
	}
}

func TestACleansOwnLockTimeoutIsWaitedOut(t *testing.T) {
	defer func(a, b time.Duration) { uvCleanWait, uvLockMargin = a, b }(uvCleanWait, uvLockMargin)
	uvCleanWait, uvLockMargin = 100*time.Millisecond, 100*time.Millisecond
	f := newUVLock(t, byPATH("archive-v0/envx"), 4, 6, 8)
	uvEntries(t, f.cache, "archive-v0/envx", "archive-v0/stale")
	o := options(t, f.proc, time.Now(), nil)
	o.afterUVLock = func() { // a uv cache clean with a longer UV_LOCK_TIMEOUT opened capacity's file, and waits for it alone
		f.addProc(t, 31, 1, "uv\x00cache\x00clean\x00", "UV_LOCK_TIMEOUT=0.4\x00")
		var st syscall.Stat_t
		must(t, syscall.Stat(f.lock, &st))
		b, err := os.ReadFile(filepath.Join(f.proc, "locks"))
		must(t, err)
		write(t, filepath.Join(f.proc, "locks"), string(b)+fmt.Sprintf("9: -> FLOCK  ADVISORY  WRITE 31 %02x:%02x:%d 0 EOF\n",
			f.id.major, f.id.minor, st.Ino))
	}
	start := time.Now()
	g := f.run(t, o)
	if !f.around(g) || !strings.Contains(g.Detail, "waited for it past 500ms") || time.Since(start) < 500*time.Millisecond {
		t.Fatalf("a clean is waited out as long as its own lock timeout: %s %q %q after %s", g.Action, g.Why, g.Detail, time.Since(start))
	}
}

func TestAStopDuringThePruneAroundWaitsForTheOldLockFile(t *testing.T) {
	seen := make(chan os.Signal, 4)
	signal.Notify(seen, syscall.SIGHUP) // the test's own, so the stop it sends does not end it
	defer signal.Stop(seen)
	f := newUVLock(t, byPATH("archive-v0/envx"), 4, 6, 8)
	uvEntries(t, f.cache, "archive-v0/envx", "archive-v0/stale")
	o := options(t, f.proc, time.Now(), nil)
	o.afterUVLock = func() { must(t, syscall.Kill(os.Getpid(), syscall.SIGHUP)) } // the old file is aside now
	if g := f.run(t, o); !f.around(g) {
		t.Fatalf("the prune runs: %s %q", g.Action, g.Why)
	}
	n := 0
	for done := time.After(2 * time.Second); n < 2; n++ {
		select {
		case <-seen:
		case <-done:
			t.Fatalf("a stop that came while the old lock file was aside is sent on once it is back: %d of 2", n)
		}
	}
}

func TestAJobThatNamesTheCacheOrAnotherBucketNamesNoEnvironment(t *testing.T) {
	for name, word := range map[string]string{"the cache": "", "another bucket": "/git-v1/checkouts"} {
		t.Run(name, func(t *testing.T) {
			f := newUVLock(t, func(cache, lock string) []fakeProc { return []fakeProc{{pid: 4, ppid: 1, argv: []string{"bash"}}} })
			uvEntries(t, f.cache, "archive-v0/stale")
			o := options(t, f.proc, time.Now(), fakeJobs{units: map[string]string{"u.service": "Environment=UV_CACHE_DIR=" +
				f.cache + word + "\nExecStart=/usr/bin/true\n"}})
			g := f.run(t, o)
			if !g.Done || !f.pruned(false) {
				t.Fatalf("a job that names the cache, or a place in no bucket of environments, leaves uv's own prune to run: %s %q %v",
					g.Action, g.Why, f.ran)
			}
		})
	}
}

func TestAJobThatStartsAnInterpreterThroughALinkKeepsItsEnvironment(t *testing.T) {
	base := t.TempDir()
	f := newUVLock(t, func(cache, lock string) []fakeProc { return nil })
	uvEntries(t, f.cache, "environments-v2/proj-cpython3.12-1", "archive-v0/stale")
	must(t, os.MkdirAll(filepath.Join(base, "proj"), 0o755))
	must(t, os.Symlink(filepath.Join(f.cache, "environments-v2", "proj-cpython3.12-1"), filepath.Join(base, "proj", ".venv")))
	o := options(t, f.proc, time.Now(), fakeJobs{units: map[string]string{"srv.service": "ExecStart=" +
		filepath.Join(base, "proj", ".venv", "bin", "python") + " /srv/serve.py\n"}})
	g := f.run(t, o)
	if !f.around(g) {
		t.Fatalf("a job that starts an environment's interpreter through a link keeps the environment: %s %q %v", g.Action, g.Why, f.ran)
	}
	left(t, f.cache, []string{"environments-v2/proj-cpython3.12-1"}, []string{"archive-v0/stale"})
}

func TestAProcessThatCameThroughAWheelsPointerKeepsThePrune(t *testing.T) {
	f := newUVLock(t, byPATH("archive-v0/envx"), 4, 6, 8)
	uvEntries(t, f.cache, "archive-v0/envx", "archive-v0/stale")
	pointer := filepath.Join(f.cache, "wheels-v6", "pypi", "pkg", "1.0-py3-none-any")
	must(t, os.MkdirAll(filepath.Dir(pointer), 0o755))
	must(t, os.Symlink("../../../archive-v0/stale", pointer))
	o := options(t, f.proc, time.Now(), nil)
	o.afterUVLock = func() { // started by a path through a wheel's pointer, which leads into archive-v0
		f.addProc(t, 14, 1, filepath.Join(pointer, "lib", "x.so")+"\x00", "")
	}
	g := f.run(t, o)
	if g.Done || g.Action != ActReport {
		t.Fatalf("a path in another bucket a later look cannot resolve keeps the prune: %s %q %v", g.Action, g.Why, f.ran)
	}
	left(t, f.cache, []string{"archive-v0/envx", "archive-v0/stale"}, nil)
}

func TestAnEntryTheUserMayNotWriteGoesAsUVWouldDeleteIt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes every directory")
	}
	// a wheel can unpack a directory without write permission; uv makes it writable before it deletes it
	f := newUVLock(t, byPATH("archive-v0/envx"), 4, 6, 8)
	uvEntries(t, f.cache, "archive-v0/envx", "archive-v0/readonly", "archive-v0/innerro")
	ro := filepath.Join(f.cache, "archive-v0", "readonly")
	inner := filepath.Join(f.cache, "archive-v0", "innerro", "lib")
	must(t, os.Chmod(ro, 0o555))
	must(t, os.Chmod(inner, 0o555))
	t.Cleanup(func() { os.Chmod(ro, 0o755); os.Chmod(inner, 0o755) })
	g := f.run(t, options(t, f.proc, time.Now(), nil))
	if !f.around(g) || strings.Contains(g.Detail, "could not") {
		t.Fatalf("an entry the user may not write goes: %s %q %q", g.Action, g.Why, g.Detail)
	}
	left(t, f.cache, []string{"archive-v0/envx"}, []string{"archive-v0/readonly", "archive-v0/innerro", uvTrash})
}

func TestAStopThatComesWhileTheLockIsAsideEndsTheRunAfterIt(t *testing.T) {
	seen := make(chan os.Signal, 2)
	signal.Notify(seen, syscall.SIGHUP) // the test's own, so the stop it sends does not end it
	defer signal.Stop(seen)
	release := holdStops()
	must(t, syscall.Kill(os.Getpid(), syscall.SIGHUP))
	select {
	case <-seen:
	case <-time.After(5 * time.Second):
		t.Fatal("the stop came")
	}
	select {
	case <-seen:
		t.Fatal("the hold relays the stop before it ends")
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case <-seen:
	case <-time.After(5 * time.Second):
		t.Fatal("the end of the hold sends the stop on")
	}
}

func TestALockFileAnEarlierRunSetAsideIsPutBack(t *testing.T) {
	for _, c := range []struct {
		name      string
		held, due bool
		half      bool // killed between the link and the rename: the file aside is the one in place
	}{
		{"nothing holds the one in place", false, true, false},
		{"no prune is due", false, false, false},
		{"a uv process holds the one in place", true, true, false},
		{"the run was killed before its rename", true, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newUVLock(t, func(cache, lock string) []fakeProc { return []fakeProc{{pid: 4, ppid: 1, argv: []string{"bash"}}} })
			if c.half {
				must(t, os.Link(f.lock, f.lock+uvAsideSuffix))
			} else { // the earlier run was killed under its lock: its file is in the lock file's place, the old one aside
				must(t, os.Rename(f.lock, f.lock+uvAsideSuffix))
				write(t, f.lock, "")
			}
			var st syscall.Stat_t
			must(t, syscall.Stat(f.lock, &st))
			if c.held {
				b, err := os.ReadFile(filepath.Join(f.proc, "locks"))
				must(t, err)
				write(t, filepath.Join(f.proc, "locks"), string(b)+fmt.Sprintf("9: FLOCK  ADVISORY  READ 40 %02x:%02x:%d 0 EOF\n",
					f.id.major, f.id.minor, st.Ino))
			}
			o := options(t, f.proc, time.Now(), nil)
			o.Tools, o.Rules = f.tools(nil), []string{RuleUV}
			store := openStore(t)
			if !c.due { // under its size, and pruned a moment ago
				o.Tools = f.tools(map[string]string{"uv cache size": "1000\n"})
				must(t, store.SetMark(context.Background(), "prune:uv", time.Now()))
			}
			res, err := Run(context.Background(), Config{Live: []string{RuleUV}}.Normal(), store, o)
			must(t, err)
			g := findings(res, RuleUV)[f.cache]
			var now syscall.Stat_t
			must(t, syscall.Stat(f.lock, &now))
			switch {
			case c.half && (exists(f.lock+uvAsideSuffix) || now.Ino != st.Ino):
				t.Fatalf("a second link to the file in place goes, and the file stays: %s %q", g.Action, g.Why)
			case c.held && !c.half && (g.Done || !strings.Contains(g.Why, "set the cache's lock file aside") || now.Ino != st.Ino):
				t.Fatalf("with the one in place held, the old file waits aside and the rule waits: %s %q", g.Action, g.Why)
			case !c.held && (now.Ino != f.id.ino || exists(f.lock+uvAsideSuffix)):
				t.Fatalf("with nothing on the one in place, the old file goes back, a prune due or not: %s %q", g.Action, g.Why)
			}
		})
	}
}

func TestWhatAnEarlierPruneMovedAsideIsDeleted(t *testing.T) {
	f := newUVLock(t, outside, 4, 6)
	write(t, filepath.Join(f.cache, uvTrash, "x1", "archive-v0", "old", "lib", "x.so"), "") // a run stopped before it deleted it
	tools := f.tools(map[string]string{"uv cache size": "1000\n"})                          // under its size: no prune is due
	o := options(t, f.proc, time.Now(), nil)
	o.Tools, o.DryRun = tools, true
	f.runWith(t, o, f.cache)
	if !exists(filepath.Join(f.cache, uvTrash)) {
		t.Fatal("a dry run deletes nothing")
	}
	o.DryRun = false
	f.runWith(t, o, f.cache)
	if exists(filepath.Join(f.cache, uvTrash)) {
		t.Error("what an earlier prune moved aside is deleted at the next run, prune or not: nothing reaches it")
	}
}

func TestABuildEnvironmentAKilledUVLeftLeavesUVsPruneToRun(t *testing.T) {
	// a build's environment holds its backend's packages; a command whose uv was killed would run from an overlay
	f := newUVLock(t, func(cache, lock string) []fakeProc { return []fakeProc{{pid: 4, ppid: 1, argv: []string{"bash"}}} })
	uvEntries(t, f.cache, "builds-v0/.tmpBuild", "archive-v0/stale")
	g := f.run(t, options(t, f.proc, time.Now(), nil))
	if !g.Done || !f.pruned(false) {
		t.Fatalf("with no overlay in builds-v0 and nothing running from the cache, uv's own prune runs: %s %q %v", g.Action, g.Why, f.ran)
	}
}

func TestWhatAJobNamesInTheCacheStays(t *testing.T) {
	f := newUVLock(t, func(cache, lock string) []fakeProc { return nil })
	uvEntries(t, f.cache, "archive-v0/envjob", "archive-v0/stale")
	o := options(t, f.proc, time.Now(), fakeJobs{units: map[string]string{"backup.service": "ExecStart=" +
		filepath.Join(f.cache, "archive-v0", "envjob", "bin", "python") + " /srv/backup.py\n"}})
	g := f.run(t, o)
	if !f.around(g) || !strings.Contains(g.Detail, "1 job names a place in it") {
		t.Fatalf("what a job names in the cache stays, with what it reaches: %s %q %q %v", g.Action, g.Why, g.Detail, f.ran)
	}
	left(t, f.cache, []string{"archive-v0/envjob"}, []string{"archive-v0/stale"})
}

func TestAnEntryThatHoldsAMountPointStays(t *testing.T) {
	f := newUVLock(t, byPATH("archive-v0/envx"), 4, 6, 8)
	uvEntries(t, f.cache, "archive-v0/envx", "archive-v0/mounted", "archive-v0/stale")
	o := options(t, f.proc, time.Now(), nil)
	write(t, o.MountInfo, "29 1 259:3 / / rw - ext4 /dev/sda2 rw\n50 29 0:44 / "+filepath.Join(f.cache, "archive-v0", "mounted", "lib")+
		" rw - tmpfs tmpfs rw\n")
	g := f.run(t, o)
	if !f.around(g) || !strings.Contains(g.Detail, "1 stay") || !strings.Contains(g.Detail, "mount point") {
		t.Fatalf("an entry that holds a mount point stays: %s %q %q", g.Action, g.Why, g.Detail)
	}
	left(t, f.cache, []string{"archive-v0/envx", "archive-v0/mounted"}, []string{"archive-v0/stale"})
}

func TestAnOverlayMadeSinceTheLookKeepsThePrune(t *testing.T) {
	f := newUVLock(t, byPATH("archive-v0/envx"), 4, 6, 8)
	uvEntries(t, f.cache, "archive-v0/envx", "archive-v0/stale")
	o := options(t, f.proc, time.Now(), nil)
	o.afterUVLock = func() { uvEntries(t, f.cache, "builds-v0/.tmpLate") }
	g := f.run(t, o)
	if g.Done || g.Action != ActReport || !strings.Contains(g.Why, ".tmpLate was made since the look") {
		t.Fatalf("an entry of builds-v0 the look did not walk keeps the prune: %s %q %v", g.Action, g.Why, f.ran)
	}
	left(t, f.cache, []string{"archive-v0/envx", "archive-v0/stale"}, nil)
}

func TestAWalkThatDoesNotEndKeepsThePrune(t *testing.T) {
	defer func(d time.Duration) { uvReachWait = d }(uvReachWait)
	uvReachWait = 100 * time.Millisecond
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	was := resolveLinks
	t.Cleanup(func() { resolveLinks = was })
	f := newUVLock(t, byPATH("archive-v0/envx"), 4, 6, 8)
	uvEntries(t, f.cache, "archive-v0/envx", "archive-v0/stale")
	dead := filepath.Join(f.cache, "archive-v0", "envx", "lib", "dead")
	must(t, os.Symlink("/mnt/dead/x", dead))
	resolveLinks = func(p string) (string, error) {
		if p == dead { // a link to a dead network mount: the call never returns
			<-stuck
		}
		return was(p)
	}
	start := time.Now()
	g := f.run(t, options(t, f.proc, time.Now(), nil))
	if g.Done || g.Action != ActReport || !strings.Contains(g.Why, "did not end in 100ms") || time.Since(start) > 10*time.Second {
		t.Fatalf("a walk that does not end leaves what they use unknown: %s %q after %s", g.Action, g.Why, time.Since(start))
	}
	left(t, f.cache, []string{"archive-v0/envx", "archive-v0/stale"}, nil)
}

func TestALaterLookThatCannotReadAProcessUnderAHolderLetsTheLockGo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every process")
	}
	f := newUVLock(t, byPATH("archive-v0/envx"), 4, 6, 8)
	uvEntries(t, f.cache, "archive-v0/envx", "archive-v0/stale")
	o := options(t, f.proc, time.Now(), nil)
	o.afterUVLock = func() { // the tool's command ran a program under sudo
		dir := f.addProc(t, 16, 9, "sudo\x00helper\x00", "")
		must(t, os.Chmod(filepath.Join(dir, "fd"), 0))
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(f.proc, "16", "fd"), 0o755) })
	g := f.run(t, o)
	if g.Done || g.Action != ActReport || !strings.Contains(g.Why, "cannot read runs under a holder of the lock (pid 16)") {
		t.Fatalf("a process a later look cannot read keeps the prune around running environments: %s %q %v", g.Action, g.Why, f.ran)
	}
	left(t, f.cache, []string{"archive-v0/envx", "archive-v0/stale"}, nil)
	letGo(t, f.lock)
}

func TestTheOverlaysPathsAreReadAsUVWritesThem(t *testing.T) {
	// uv writes each path as the bytes of a Python bytes literal, escaped as Rust's escape_ascii escapes them
	line := `import site; site.addsitedir(__import__("os").fsdecode(b"/c/uv/archive-v0/x/lib/python3.12/site-packages")); ` +
		`site.addsitedir(__import__("os").fsdecode(b"/home/u/caf\xc3\xa9 \"q\"/.venv/lib/python3.12/site-packages"))`
	got := pthPaths([]byte("# made by uv\n"+line+"\nrel/dir\n/abs/dir\n"), "/env/lib/python3.12/site-packages", false)
	want := []string{"/c/uv/archive-v0/x/lib/python3.12/site-packages", "/home/u/café \"q\"/.venv/lib/python3.12/site-packages",
		"/env/lib/python3.12/site-packages/rel/dir", "/abs/dir"}
	if !slices.Equal(got, want) {
		t.Errorf("paths %q, want %q", got, want)
	}
	if got := cfgPaths([]byte("home = /opt/py/bin\nimplementation = CPython\nextends-environment = /proj/.venv\n")); !slices.Equal(got,
		[]string{"/opt/py/bin", "/proj/.venv"}) {
		t.Errorf("pyvenv.cfg's paths: %q", got)
	}
}

func TestAPruneAroundTakesUVsOwnLockWhenNobodyHoldsIt(t *testing.T) {
	// a tool server whose uv ended runs on from its cached environment, and no uv process holds the lock
	daemon := func(cache, lock string) []fakeProc {
		return []fakeProc{{pid: 9, ppid: 1, argv: []string{filepath.Join(cache, "archive-v0", "envd", "bin", "python"), "srv"}}}
	}
	f := newUVLock(t, daemon)
	uvEntries(t, f.cache, "archive-v0/envd", "archive-v0/stale")
	o := options(t, f.proc, time.Now(), nil)
	held := false
	o.afterUVLock = func() {
		fh, err := os.Open(f.lock)
		must(t, err)
		defer fh.Close()
		held = syscall.Flock(int(fh.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) != nil
	}
	g := f.run(t, o)
	if !f.around(g) || !held {
		t.Fatalf("with nobody on uv's lock, the prune around takes it, as uv's own prune does (held %v): %s %q %v", held, g.Action, g.Why, f.ran)
	}
	left(t, f.cache, []string{"archive-v0/envd"}, []string{"archive-v0/stale"})
	if id, err := fileID(f.lock); err != nil || id != f.id {
		t.Errorf("uv's own lock file stays in place: %v %v", id, err)
	}
	letGo(t, f.lock)

	f = newUVLock(t, daemon)
	uvEntries(t, f.cache, "archive-v0/envd", "archive-v0/stale")
	holder, err := os.OpenFile(f.lock, os.O_RDWR, 0)
	must(t, err)
	defer holder.Close()
	must(t, syscall.Flock(int(holder.Fd()), syscall.LOCK_SH)) // a uv process that started after the look read the locks
	g = f.run(t, options(t, f.proc, time.Now(), nil))
	if g.Done || g.Action != ActReport || !strings.Contains(g.Why, "took the cache's lock since the look") {
		t.Fatalf("a uv process that took the lock since the look keeps the prune: %s %q %v", g.Action, g.Why, f.ran)
	}
	left(t, f.cache, []string{"archive-v0/envd", "archive-v0/stale"}, nil)
}

func TestAProcessThatCameToUseAnEntryThePruneWouldDeleteKeepsIt(t *testing.T) {
	daemon := func(cache, lock string) []fakeProc {
		return []fakeProc{{pid: 9, ppid: 1, argv: []string{"srv"}, env: started(filepath.Join(cache, "archive-v0", "envx", "bin"))}}
	}
	for name, c := range map[string]struct {
		procs   func(cache, lock string) []fakeProc
		holders []int
	}{
		"under a lock in uv's lock file's place": {byPATH("archive-v0/envx"), []int{4, 6, 8}},
		"under uv's own lock":                    {daemon, nil},
	} {
		t.Run(name, func(t *testing.T) {
			f := newUVLock(t, c.procs, c.holders...)
			uvEntries(t, f.cache, "archive-v0/envx", "archive-v0/stale")
			o := options(t, f.proc, time.Now(), nil)
			o.afterUVLock = func() {
				dir := f.addProc(t, 12, 1, "python3\x00", "")
				write(t, filepath.Join(dir, "maps"), "7f-7f r--s 0 0:0 1 "+filepath.Join(f.cache, "archive-v0", "stale", "lib", "x.so")+"\n")
			}
			g := f.run(t, o)
			if g.Done || g.Action != ActReport || !strings.Contains(g.Why, "python3 (pid 12)") {
				t.Fatalf("a process that came to use an entry the prune would delete keeps it: %s %q %v", g.Action, g.Why, f.ran)
			}
			left(t, f.cache, []string{"archive-v0/envx", "archive-v0/stale"}, nil)
			letGo(t, f.lock)
		})
	}
}

func TestALaterLookThatSeesMoreOfWhatIsKeptPrunes(t *testing.T) {
	f := newUVLock(t, byPATH("archive-v0/envx"), 4, 6, 8)
	uvEntries(t, f.cache, "archive-v0/envx", "archive-v0/stale")
	o := options(t, f.proc, time.Now(), nil)
	o.afterUVLock = func() {
		// the tool's command maps another file of its environment, and a shell sits in another bucket of the cache
		write(t, filepath.Join(f.proc, "9", "maps"), "7f-7f r--s 0 0:0 1 "+filepath.Join(f.cache, "archive-v0", "envx", "lib", "x.so")+"\n")
		f.addProc(t, 13, 1, "bash\x00", "")
		must(t, os.Symlink(filepath.Join(f.cache, "git-v1", "checkouts", "x"), filepath.Join(f.proc, "13", "cwd")))
	}
	g := f.run(t, o)
	if !f.around(g) {
		t.Fatalf("what a later look finds of what the first kept, or of a bucket nothing goes from, keeps no prune: %s %q %v", g.Action, g.Why, f.ran)
	}
	left(t, f.cache, []string{"archive-v0/envx"}, []string{"archive-v0/stale"})
}

func TestALockFileThatChangedSinceTheLookIsNotTakenAroundRunningEnvironments(t *testing.T) {
	daemon := func(cache, lock string) []fakeProc {
		return []fakeProc{{pid: 9, ppid: 1, argv: []string{filepath.Join(cache, "archive-v0", "envd", "bin", "python"), "srv"}}}
	}
	f := newUVLock(t, daemon)
	uvEntries(t, f.cache, "archive-v0/envd", "archive-v0/stale")
	armed := false
	o := options(t, f.proc, time.Now(), nil)
	o.Tools = dockerHook{f.tools(nil), &armed, func() { // a clean of uv's put a new lock file in place after the look
		write(t, f.lock+".new", "")
		must(t, os.Rename(f.lock+".new", f.lock))
	}}
	g := f.runWith(t, o, f.cache)
	if g.Done || g.Action != ActReport || !strings.Contains(g.Why, "changed since the look") {
		t.Fatalf("a lock file that is not the one the look saw is not taken: %s %q %v", g.Action, g.Why, f.ran)
	}
	left(t, f.cache, []string{"archive-v0/envd", "archive-v0/stale"}, nil)
}

func TestAPruneAroundStartsNoEntryPastItsTime(t *testing.T) {
	defer func(d time.Duration) { uvAroundFor = d }(uvAroundFor)
	uvAroundFor = 0
	f := newUVLock(t, byPATH("archive-v0/envx"), 4, 6, 8)
	uvEntries(t, f.cache, "archive-v0/envx", "archive-v0/stale", "environments-v2/s-2")
	g := f.run(t, options(t, f.proc, time.Now(), nil))
	if !f.around(g) || !strings.Contains(g.Detail, "left 2 for a later run") {
		t.Fatalf("past its time the prune starts no entry, and says what it left: %s %q %q", g.Action, g.Why, g.Detail)
	}
	left(t, f.cache, []string{"archive-v0/envx", "archive-v0/stale", "environments-v2/s-2"}, nil)
	letGo(t, f.lock)
}

func TestAnEntryThatCannotBeRemovedIsNamedAndTheRestGo(t *testing.T) {
	f := newUVLock(t, byPATH("archive-v0/envx"), 4, 6, 8)
	uvEntries(t, f.cache, "archive-v0/envx", "archive-v0/busy", "archive-v0/stale")
	fh, err := os.Open(filepath.Join(f.cache, "archive-v0", "busy"))
	must(t, err)
	defer fh.Close()
	must(t, syscall.Flock(int(fh.Fd()), syscall.LOCK_SH)) // another process holds a lock on the directory
	o := options(t, f.proc, time.Now(), nil)
	o.Tools, o.Rules = f.tools(nil), []string{RuleUV}
	res, err := Run(context.Background(), Config{Live: []string{RuleUV}}.Normal(), openStore(t), o)
	must(t, err)
	got := findings(res, RuleUV)
	g, busy := got[f.cache], got[filepath.Join(f.cache, "archive-v0", "busy")]
	if !f.around(g) || !strings.Contains(g.Detail, "1 could not be removed") || !strings.Contains(g.Detail, "busy") {
		t.Fatalf("an entry that cannot go is named, and the rest go: %s %q %q", g.Action, g.Why, g.Detail)
	}
	if busy.Err == "" || busy.Done {
		t.Errorf("the entry that could not go is a failed deletion of its own: %+v", busy)
	}
	left(t, f.cache, []string{"archive-v0/envx", "archive-v0/busy"}, []string{"archive-v0/stale"})
}

func TestOnlyTheLinksToKeptEnvironmentsStay(t *testing.T) {
	f := newUVLock(t, byPATH("archive-v0/envkept"), 4, 6, 8)
	uvEntries(t, f.cache, "archive-v0/envkept", "archive-v0/envstale")
	s1, s2 := filepath.Join(f.cache, "environments-v2", "1f34641d6828a9ac"), filepath.Join(f.cache, "environments-v2", "849831b06a90a36f")
	must(t, os.MkdirAll(s1, 0o755))
	must(t, os.MkdirAll(s2, 0o755))
	must(t, os.Symlink("../../archive-v0/envkept", filepath.Join(s1, "a")))
	must(t, os.Symlink("../../archive-v0/envstale", filepath.Join(s1, "b")))
	must(t, os.Symlink("../../archive-v0/gone", filepath.Join(s1, "c"))) // to an environment that is gone
	write(t, filepath.Join(s1, "notes"), "")                             // no link: not uv's
	must(t, os.Symlink("../../archive-v0/envstale", filepath.Join(s2, "d")))
	write(t, filepath.Join(f.cache, "archive-v0", ".partial", "x"), "") // no entry uv made
	write(t, filepath.Join(f.cache, "environments-v2", "notes"), "")    // nor is a file beside the environments
	g := f.run(t, options(t, f.proc, time.Now(), nil))
	if !f.around(g) {
		t.Fatalf("the prune runs around the environment: %s %q %v", g.Action, g.Why, f.ran)
	}
	left(t, f.cache, []string{"archive-v0/envkept", "environments-v2/1f34641d6828a9ac/a", "environments-v2/1f34641d6828a9ac/notes",
		"archive-v0/.partial", "environments-v2/notes"}, []string{"archive-v0/envstale", "environments-v2/1f34641d6828a9ac/b",
		"environments-v2/1f34641d6828a9ac/c", "environments-v2/849831b06a90a36f"})
}

func TestAFileOfTheContentAddressedCacheGoesWithTheLastArchiveThatLinksIt(t *testing.T) {
	// uv 0.13's content-addressed cache (a preview) keeps a wheel's files in files-v0 and hardlinks them into its
	// archive; an environment's files are links of those
	f := newUVLock(t, byPATH("archive-v0/envx"), 4, 6, 8)
	uvEntries(t, f.cache, "archive-v0/envx")
	files := filepath.Join(f.cache, "files-v0")
	shared, alone, orphan := filepath.Join(files, "ab", "cdef"), filepath.Join(files, "12", "3456"), filepath.Join(files, "99", "7777")
	for _, p := range []string{shared, alone, orphan} {
		write(t, p, "x = 1\n")
	}
	for link, obj := range map[string]string{
		"archive-v0/wheel-a/pkg/a.py": shared, "archive-v0/envx/lib/python3.12/site-packages/pkg/a.py": shared,
		"archive-v0/wheel-b/pkg/b.py": alone,
	} {
		must(t, os.MkdirAll(filepath.Dir(filepath.Join(f.cache, link)), 0o755))
		must(t, os.Link(obj, filepath.Join(f.cache, link)))
	}
	g := f.run(t, options(t, f.proc, time.Now(), nil))
	if !f.around(g) || !strings.Contains(g.Detail, "1 file of files-v0") {
		t.Fatalf("a file no archive links goes: %s %q %q", g.Action, g.Why, g.Detail)
	}
	// an archive moved aside links its files until it is deleted, after the lock: they go at the next prune
	left(t, f.cache, []string{"archive-v0/envx", "files-v0/ab/cdef", "files-v0/12/3456"},
		[]string{"archive-v0/wheel-a", "archive-v0/wheel-b", "files-v0/99"})
	if g := f.run(t, options(t, f.proc, time.Now(), nil)); !f.around(g) || !strings.Contains(g.Detail, "1 file of files-v0") {
		t.Fatalf("the next prune removes the files the archives that went alone linked: %s %q %q", g.Action, g.Why, g.Detail)
	}
	left(t, f.cache, []string{"archive-v0/envx", "files-v0/ab/cdef"}, []string{"files-v0/12"})
}

func TestAProcessThatUsesABucketItselfIsWaitedFor(t *testing.T) {
	f := newUVLock(t, func(cache, lock string) []fakeProc {
		return append(outside(cache, lock), fakeProc{pid: 12, ppid: 1, argv: []string{"bash"}, cwd: filepath.Join(cache, "archive-v0")})
	}, 4, 6)
	uvEntries(t, f.cache, "archive-v0/stale")
	g := f.run(t, options(t, f.proc, time.Now(), nil))
	if g.Done || g.Action != ActReport || !strings.Contains(g.Why, "bash (pid 12) uses") || !strings.Contains(g.Why, "archive-v0 itself") {
		t.Fatalf("a process in a bucket the prune deletes from is waited for: %s %q %v", g.Action, g.Why, f.ran)
	}
	left(t, f.cache, []string{"archive-v0/stale"}, nil)
}

func TestADirectoryOfAKeptEnvironmentTheRunCannotReadIsWaitedFor(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every directory")
	}
	f := newUVLock(t, byPATH("archive-v0/envx"), 4, 6, 8)
	uvEntries(t, f.cache, "archive-v0/envx", "archive-v0/stale")
	hidden := filepath.Join(f.cache, "archive-v0", "envx", "lib", "python3.12")
	must(t, os.MkdirAll(hidden, 0o755))
	must(t, os.Chmod(hidden, 0))
	t.Cleanup(func() { os.Chmod(hidden, 0o755) })
	g := f.run(t, options(t, f.proc, time.Now(), nil))
	if g.Done || g.Action != ActReport || !strings.Contains(g.Why, "cannot be read") {
		t.Fatalf("what a kept environment reaches through a directory the run cannot read is not known: %s %q %v", g.Action, g.Why, f.ran)
	}
	left(t, f.cache, []string{"archive-v0/envx", "archive-v0/stale"}, nil)
}

func TestADryRunSaysWhatAPruneAroundKeeps(t *testing.T) {
	f := newUVLock(t, byPATH("archive-v0/envx"), 4, 6, 8)
	uvEntries(t, f.cache, "archive-v0/envx", "archive-v0/stale")
	blob(t, filepath.Join(f.cache, "archive-v0", "envx", "lib", "big.so"), 3<<20)
	blob(t, filepath.Join(f.cache, "builds-v0", ".tmpB", "lib", "big.so"), 2<<20)
	o := options(t, f.proc, time.Now(), nil)
	o.DryRun = true
	g := f.run(t, o)
	if g.Done || g.Action != ActDelete || !strings.Contains(g.Detail, "it keeps 1 environment and what they reach (3 MiB), and builds-v0 (2 MiB)") {
		t.Fatalf("a dry run says the prune would run around the environment: %s %q %q", g.Action, g.Why, g.Detail)
	}
	left(t, f.cache, []string{"archive-v0/envx", "archive-v0/stale"}, nil)
}
