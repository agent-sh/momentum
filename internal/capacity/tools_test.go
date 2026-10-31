package capacity

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/agent-sh/momentum/internal/store"
)

func uvTools(size string, ran *[]string) fakeTools {
	return fakeTools{ran: ran, answers: map[string]string{
		"uv cache dir": "/home/u/.cache/uv\n", "uv cache size": size + "\n", "docker": "ERR"}}
}

func TestTheUVCacheIsPrunedOverItsSizeButNeverDuringAnInstall(t *testing.T) {
	now := time.Now()
	cfg := Config{Live: []string{RuleUV}}.Normal()
	over := "40000000000" // 37 GiB, over the 30 GiB default
	for _, c := range []struct {
		name   string
		procs  []fakeProc
		prunes bool
		action string
	}{
		{"no uv process runs", []fakeProc{{pid: 4, ppid: 1, argv: []string{"bash"}}}, true, ActDelete},
		{"a pip install runs", []fakeProc{{pid: 4, ppid: 1, argv: []string{"/usr/bin/uv", "pip", "install", "x"}}}, false, ActKeep},
		{"a uv run still syncs", []fakeProc{{pid: 4, ppid: 1, argv: []string{"uv", "run", "server"}}}, false, ActKeep},
		{"a uv run past its sync, outside the cache", []fakeProc{{pid: 4, ppid: 1, argv: []string{"uv", "run", "server"}},
			{pid: 5, ppid: 4, argv: []string{"python", "server.py"}, exe: "/proj/.venv/bin/python"}}, true, ActDelete},
		{"a uvx tool server runs from the cache", []fakeProc{{pid: 4, ppid: 1, argv: []string{"uvx", "mcp-x"}},
			{pid: 5, ppid: 4, argv: []string{"/home/u/.cache/uv/archive-v0/x/bin/python", "mcp-x"},
				exe: "/home/u/.cache/uv/archive-v0/x/bin/python"}}, false, ActReport},
		{"a process maps a file from the cache", []fakeProc{{pid: 6, ppid: 1, argv: []string{"python3"},
			maps: []string{"/home/u/.cache/uv/archive-v0/abc/lib.so"}}}, false, ActReport},
	} {
		t.Run(c.name, func(t *testing.T) {
			var ran []string
			o := options(t, procRoot(t, c.procs...), now, nil)
			o.Tools = uvTools(over, &ran)
			o.Rules = []string{RuleUV}
			st := openStore(t)
			res, err := Run(context.Background(), cfg, st, o)
			if err != nil {
				t.Fatal(err)
			}
			pruned := strings.Contains(strings.Join(ran, "\n"), "uv cache prune --ci")
			if pruned != c.prunes {
				t.Fatalf("pruned %v, want %v: %v %+v", pruned, c.prunes, ran, res.Rules)
			}
			if strings.Contains(strings.Join(ran, "\n"), "--force") {
				t.Fatalf("with no holder of the lock, a prune takes uv's own: %v", ran)
			}
			if f := findings(res, RuleUV)["/home/u/.cache/uv"]; f.Action != c.action {
				t.Errorf("action %q (%s), want %q", f.Action, f.Why, c.action)
			}
			if mark, _ := st.Mark(context.Background(), "prune:uv"); mark.IsZero() == c.prunes {
				t.Errorf("a prune is marked, a wait is not: %v", mark)
			}
		})
	}
}

// shrinking answers uv's cache size with before until a prune runs, then with after.
type shrinking struct {
	fakeTools
	after  string
	pruned *bool
}

func (s shrinking) UV(ctx context.Context, d time.Duration, args ...string) (string, error) {
	switch cmd := strings.Join(args, " "); {
	case strings.HasPrefix(cmd, "cache prune"):
		*s.pruned = true
	case strings.HasPrefix(cmd, "cache size") && *s.pruned:
		return s.after + "\n", nil
	}
	return s.fakeTools.UV(ctx, d, args...)
}

func TestAUVPruneIsADecidedNoteOnlyWhenItFreedSomething(t *testing.T) {
	for _, c := range []struct {
		before, after string
		notes         int
	}{
		{"40000000000", "25000000000", 1},
		{"40000000000", "40000000000", 0},
		{"no count", "no count", 1}, // uv cannot count it: a walk of the cache cannot tell that nothing went
	} {
		pruned := false
		cfg := Config{Live: []string{RuleUV}}.Normal()
		o := options(t, procRoot(t), time.Now(), nil)
		o.Tools, o.Rules = shrinking{uvTools(c.before, nil), c.after, &pruned}, []string{RuleUV}
		st := openStore(t)
		ctx := context.Background()
		res, err := Run(ctx, cfg, st, o)
		if err != nil {
			t.Fatal(err)
		}
		if f := findings(res, RuleUV)["/home/u/.cache/uv"]; !f.Done {
			t.Fatalf("the prune ran: %+v", f)
		}
		ns, err := st.Notes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, x := range ns {
			if x.Kind == store.NoteDecided {
				n++
			}
		}
		if n != c.notes {
			t.Errorf("down to %s bytes: %d decided notes, want %d", c.after, n, c.notes)
		}
	}
}

func TestAPruneUVRefusesForItsLockIsReportedNotRetriedPastIt(t *testing.T) {
	var ran []string
	tools := fakeTools{ran: &ran, answers: map[string]string{"uv cache dir": "/home/u/.cache/uv\n", "uv cache size": "40000000000\n",
		"uv cache prune": "ERR-INUSE", "docker": "ERR"}}
	cfg := Config{Live: []string{RuleUV}}.Normal()
	o := options(t, procRoot(t), time.Now(), nil)
	o.Tools, o.Rules = tools, []string{RuleUV}
	st := openStore(t)
	res, err := Run(context.Background(), cfg, st, o)
	if err != nil {
		t.Fatal(err)
	}
	f := findings(res, RuleUV)["/home/u/.cache/uv"]
	if f.Action != ActReport || f.class != "busy" || f.Done {
		t.Fatalf("a uv process that holds the cache's lock keeps it, and the cache is reported: %+v", f)
	}
	if mark, _ := st.Mark(context.Background(), "prune:uv"); !mark.IsZero() {
		t.Error("a refused prune is no prune")
	}
}

func TestTheUVCacheUnderItsSizeIsPrunedWeekly(t *testing.T) {
	now := time.Now()
	cfg := Config{Live: []string{RuleUV}}.Normal()
	ctx := context.Background()
	st := openStore(t)
	for _, c := range []struct {
		last   time.Time
		prunes bool
	}{
		{now.Add(-2 * 24 * time.Hour), false},
		{now.Add(-8 * 24 * time.Hour), true},
	} {
		must(t, st.SetMark(ctx, "prune:uv", c.last))
		var ran []string
		o := options(t, procRoot(t), now, nil)
		o.Tools, o.Rules, o.Now = uvTools("1000", &ran), []string{RuleUV}, true
		if _, err := Run(ctx, cfg, st, o); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(strings.Join(ran, "\n"), "prune"); got != c.prunes {
			t.Errorf("last prune %s ago: pruned %v", now.Sub(c.last).Round(time.Hour), got)
		}
	}
}

func TestDockerPrunesOldBuildCacheAndDanglingImagesAndOnlyReportsVolumes(t *testing.T) {
	var ran []string
	tools := fakeTools{ran: &ran, answers: map[string]string{
		"uv":                 "ERR",
		"docker info":        "29.0.0\n",
		"docker builder du":  duRecords,
		"docker image ls":    "sha256:1\t2GB\n",
		"docker system df":   `[{"Name":"db","Links":"0","Size":"3GB"},{"Name":"live","Links":"1","Size":"9GB"}]`,
		"docker builder":     "Total:\t1.9GB\n",
		"docker image prune": "Total reclaimed space: 2GB\n",
	}}
	cfg := Config{Live: []string{RuleDocker}}.Normal()
	o := options(t, procRoot(t), time.Now(), nil)
	o.Tools, o.Rules = tools, []string{RuleDocker}
	st := openStore(t)
	res, err := Run(context.Background(), cfg, st, o)
	if err != nil {
		t.Fatal(err)
	}
	got := findings(res, RuleDocker)
	if f := got["docker build cache"]; !f.Done || f.Bytes != 2e9 || f.Freed != 1.9e9 || !strings.Contains(f.Detail, "2 records") {
		t.Errorf("build cache: the old private records (2 GB), freed what Docker says: %+v", f)
	}
	if f := got["docker dangling images"]; !f.Done || f.Freed != 2e9 {
		t.Errorf("dangling images: %+v", f)
	}
	if f := got["docker volumes"]; f.Action != ActReport || f.Bytes != 3e9 {
		t.Errorf("only the unused volume is reported, and nothing prunes volumes: %+v", f)
	}
	for _, c := range ran {
		if strings.Contains(c, "volume") && strings.Contains(c, "prune") {
			t.Errorf("volumes are never pruned: %s", c)
		}
	}
	if !strings.Contains(strings.Join(ran, "\n"), "docker builder prune --force --filter until=168h") {
		t.Errorf("the build cache prune is limited by age: %v", ran)
	}
	if !strings.Contains(strings.Join(ran, "\n"), "docker builder du --format json") {
		t.Errorf("the size is read from the builder the prune runs on: %v", ran)
	}
	evs, err := st.Events(context.Background(), store.Query{Source: store.Capacity, Kinds: []string{KindPrune}})
	if err != nil || len(evs) != 2 {
		t.Errorf("each prune is an event: %d %v", len(evs), err)
	}
}

// duRecords is `docker builder du --format json`: of the build cache, a prune limited to 168 hours removes the
// 1.5 GB record last used 9 days ago and the 500 MB one never used, and nothing else. The old base under a child
// used 2 hours ago stays with its child, and so does its own base.
const duRecords = `{"ID":"old","LastUsedAt":"9 days ago","Reclaimable":true,"Shared":false,"Size":"1.5GB","Type":"regular"}
{"ID":"root","Parents":null,"LastUsedAt":"2 months ago","Reclaimable":true,"Shared":false,"Size":"3GB","Type":"regular"}
{"ID":"base","Parents":["root"],"LastUsedAt":"30 days ago","Reclaimable":true,"Shared":false,"Size":"1GB","Type":"regular"}
{"ID":"child","Parents":["base"],"LastUsedAt":"2 hours ago","Reclaimable":true,"Shared":false,"Size":"20MB","Type":"regular"}
{"ID":"never","LastUsedAt":"","Reclaimable":true,"Shared":false,"Size":"500MB","Type":"exec.cachemount"}
{"ID":"shared","LastUsedAt":"12 days ago","Reclaimable":true,"Shared":true,"Size":"30GB","Type":"regular"}
{"ID":"recent","LastUsedAt":"6 days ago","Reclaimable":true,"Shared":false,"Size":"20GB","Type":"regular"}
{"ID":"inuse","LastUsedAt":"3 weeks ago","Reclaimable":false,"Shared":false,"Size":"4GB","Type":"regular"}
{"ID":"internal","LastUsedAt":"2 months ago","Reclaimable":true,"Shared":false,"Size":"1GB","Type":"internal"}
{"ID":"frontend","LastUsedAt":"2 months ago","Reclaimable":true,"Shared":false,"Size":"1GB","Type":"frontend"}
{"ID":"odd","LastUsedAt":"a while ago","Reclaimable":true,"Shared":false,"Size":"7GB","Type":"regular"}
`

func TestAPruneThatCanFreeNothingDoesNotRunAndOneThatRemovedNothingIsNoNote(t *testing.T) {
	removed := "ID                          RECLAIMABLE   SIZE   LAST ACCESSED\nold   true   0B   9 days ago\nTotal:\t0B\n"
	for _, c := range []struct {
		name, du, said string
		runs           bool
		notes          int
		effect         string
	}{
		{"only shared and recent records", `{"ID":"s","LastUsedAt":"12 days ago","Reclaimable":true,"Shared":true,"Size":"30GB","Type":"regular"}
{"ID":"r","LastUsedAt":"About an hour ago","Reclaimable":true,"Shared":false,"Size":"20GB","Type":"regular"}`, "", false, 0, ""},
		{"Docker removes nothing", duRecords, "Total:\t0B\n", true, 0, ""},
		{"Docker removes empty records", duRecords, removed, true, 1, "Freed 0 MiB"},
		{"Docker prints no total", duRecords, "", true, 1, "did not print"},
		// a size that cannot be read is no reason to stop Docker's own prune
		{"du fails", "ERR", "Total:\t1GB\n", true, 1, "Freed 954 MiB"},
		{"du prints nothing this build reads", "ID  RECLAIMABLE  SIZE\nx  true  1GB\n", "Total:\t1GB\n", true, 1, "Freed 954 MiB"},
		// the line it cannot read may be the child that holds the old base
		{"du prints a line this build cannot read", `{"ID":"base","LastUsedAt":"30 days ago","Reclaimable":true,"Shared":false,"Size":"1GB","Type":"regular"}
{"ID":"child","Parents":["base"],"LastUsedAt":2}`, "Total:\t0B\n", true, 0, ""},
	} {
		var ran []string
		tools := fakeTools{ran: &ran, answers: map[string]string{"uv": "ERR", "docker info": "29.0.0\n", "docker builder du": c.du,
			"docker image ls": "", "docker system df": "[]", "docker builder prune": c.said}}
		cfg := Config{Live: []string{RuleDocker}}.Normal()
		o := options(t, procRoot(t), time.Now(), nil)
		o.Tools, o.Rules = tools, []string{RuleDocker}
		st := openStore(t)
		ctx := context.Background()
		res, err := Run(ctx, cfg, st, o)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(strings.Join(ran, "\n"), "builder prune"); got != c.runs {
			t.Errorf("%s: pruned %v", c.name, got)
		}
		unknown := c.du == "ERR" || strings.Contains(c.name, "this build")
		if f := findings(res, RuleDocker)["docker build cache"]; unknown && !strings.Contains(f.Detail, "size unknown") {
			t.Errorf("%s: says its size is unknown: %+v", c.name, f)
		}
		evs, err := st.Events(ctx, store.Query{Source: store.Capacity, Kinds: []string{KindPrune}})
		if err != nil || len(evs) != map[bool]int{true: 1}[c.runs] {
			t.Errorf("%s: a prune that ran is an event: %d %v", c.name, len(evs), err)
		}
		for _, e := range evs {
			if unknown != strings.Contains(e.Summary, "size unknown") || (c.said == "") != strings.Contains(e.Summary, "did not print") {
				t.Errorf("%s: the event says what is not known: %s", c.name, e.Summary)
			}
		}
		ns, err := st.Notes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, x := range ns {
			if x.Kind == store.NoteDecided {
				n++
				if !strings.Contains(x.Effect, c.effect) {
					t.Errorf("%s: effect %q, want %q", c.name, x.Effect, c.effect)
				}
			}
		}
		if n != c.notes {
			t.Errorf("%s: %d decided notes, want %d", c.name, n, c.notes)
		}
	}
}

func TestARecordGoesFromItsAgeOnAtTheLeastItCanBe(t *testing.T) {
	rec := `{"ID":"a","LastUsedAt":"7 days ago","Reclaimable":true,"Shared":false,"Size":"1GB","Type":"regular"}`
	if n, _, _ := prunable(rec, 167*time.Hour+30*time.Minute); n != 1 {
		t.Error("7 days is at least 167h30m: a record that old goes at that age")
	}
	if n, _, _ := prunable(rec, 167*time.Hour+31*time.Minute); n != 0 {
		t.Error("7 days may be less than 167h31m: the record waits")
	}
}

func TestAgesReadAsDockerPrintsThem(t *testing.T) {
	day := 24 * time.Hour
	half := 30 * time.Minute
	for in, want := range map[string]time.Duration{"Less than a second ago": 0, "1 second ago": time.Second,
		"12 seconds ago": 12 * time.Second, "About a minute ago": time.Minute, "45 minutes ago": 45 * time.Minute,
		"About an hour ago": time.Hour, "2 hours ago": 2*time.Hour - half, "47 hours ago": 47*time.Hour - half,
		"7 days ago": 7*day - half, "2 weeks ago": 14*day - half, "3 months ago": 90*day - half, "2 years ago": 730 * day} {
		if got, ok := humanAge(in); !ok || got != want {
			t.Errorf("humanAge(%q) = %v %v, want %v", in, got, ok, want)
		}
	}
	if _, ok := humanAge("a while ago"); ok {
		t.Error("an age Docker does not print is not read")
	}
}

func TestSizesReadAsDockerPrintsThem(t *testing.T) {
	for in, want := range map[string]int64{"1.785GB": 1785000000, "4.096kB": 4096, "512B": 512, "2GiB": 2 << 30, "junk": 0} {
		if got := parseSize(in); got != want {
			t.Errorf("parseSize(%q) = %d, want %d", in, got, want)
		}
	}
}

// uvLockFixture is a uv cache with its lock file, a /proc whose locks file says which processes hold it, and a uv
// that checks, as it prunes, whether the file in the lock file's place is held exclusively.
type uvLockFixture struct {
	cache, lock, proc string
	id                lockID
	ran               []string
	lockedAtPrune     *bool
}

// newUVLock makes the cache, then the processes procs gives for it, of which holders hold its lock.
func newUVLock(t *testing.T, procs func(cache, lock string) []fakeProc, holders ...int) *uvLockFixture {
	t.Helper()
	f := &uvLockFixture{cache: filepath.Join(t.TempDir(), "uv"), lockedAtPrune: new(bool)}
	f.lock = filepath.Join(f.cache, ".lock")
	write(t, f.lock, "")
	must(t, os.Chmod(f.lock, 0o666))
	var st syscall.Stat_t
	must(t, syscall.Stat(f.lock, &st))
	f.id = lockID{unix.Major(uint64(st.Dev)), unix.Minor(uint64(st.Dev)), st.Ino}
	f.proc = procRoot(t, procs(f.cache, f.lock)...)
	var locks strings.Builder
	for i, pid := range holders {
		fmt.Fprintf(&locks, "%d: FLOCK  ADVISORY  READ %d %02x:%02x:%d 0 EOF\n", i+1, pid, f.id.major, f.id.minor, f.id.ino)
	}
	// a prune waiting for the lock holds nothing, and a lock on another file is another's
	fmt.Fprintf(&locks, "%d: -> FLOCK  ADVISORY  WRITE 99 %02x:%02x:%d 0 EOF\n", len(holders), f.id.major, f.id.minor, f.id.ino)
	fmt.Fprintf(&locks, "%d: FLOCK  ADVISORY  WRITE 555 %02x:%02x:%d 0 EOF\n", len(holders)+1, f.id.major, f.id.minor, f.id.ino+1)
	write(t, filepath.Join(f.proc, "locks"), locks.String())
	return f
}

// holds adds a holder of the lock file's first inode to the locks file.
func (f *uvLockFixture) holds(t *testing.T, pid int) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.proc, "locks"))
	must(t, err)
	write(t, filepath.Join(f.proc, "locks"), string(b)+fmt.Sprintf("9: FLOCK  ADVISORY  READ %d %02x:%02x:%d 0 EOF\n", pid,
		f.id.major, f.id.minor, f.id.ino))
}

type lockCheckingUV struct {
	fakeTools
	f *uvLockFixture
}

func (u lockCheckingUV) UV(ctx context.Context, d time.Duration, args ...string) (string, error) {
	if strings.HasPrefix(strings.Join(args, " "), "cache prune") {
		if fh, err := os.Open(u.f.lock); err == nil {
			*u.f.lockedAtPrune = syscall.Flock(int(fh.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) != nil
			fh.Close()
		}
	}
	return u.fakeTools.UV(ctx, d, args...)
}

func (f *uvLockFixture) run(t *testing.T, o Options) Finding {
	t.Helper()
	o.Tools = lockCheckingUV{fakeTools{ran: &f.ran, answers: map[string]string{"uv cache dir": f.cache + "\n",
		"uv cache size": "40000000000\n", "docker": "ERR"}}, f}
	o.Rules = []string{RuleUV}
	res, err := Run(context.Background(), Config{Live: []string{RuleUV}}.Normal(), openStore(t), o)
	if err != nil {
		t.Fatal(err)
	}
	return findings(res, RuleUV)[f.cache]
}

func (f *uvLockFixture) pruned(force bool) bool {
	for _, c := range f.ran {
		if strings.HasPrefix(c, "uv cache prune --ci") && strings.Contains(c, "--force") == force {
			return true
		}
	}
	return false
}

// outside are two uv processes that hold the lock and run outside the cache: an installed tool, and a project's run
// with its own environment. A holder's one file in the cache is its lock.
func outside(cache, lock string) []fakeProc {
	return []fakeProc{{pid: 4, ppid: 1, argv: []string{"uv", "tool", "run", "pkg"}, fds: []string{lock}},
		{pid: 5, ppid: 4, argv: []string{"/opt/tools/pkg/bin/python", "pkg"}, exe: "/opt/python/bin/python3",
			env: []string{"PATH=/opt/tools/pkg/bin:/usr/bin", "HOME=/home/u"}},
		{pid: 6, ppid: 1, argv: []string{"uv", "run", "server"}, fds: []string{lock}},
		{pid: 7, ppid: 6, argv: []string{"python", "server.py"}, exe: "/opt/python/bin/python3",
			env: []string{"VIRTUAL_ENV=/proj/.venv", "PATH=/proj/.venv/bin:/usr/bin"}}}
}

// fromCache is a uv process that holds the lock and a command of it that runs from the cache, as cmd says.
func fromCache(cmd func(cache string) fakeProc) func(cache, lock string) []fakeProc {
	return func(cache, lock string) []fakeProc {
		c := cmd(cache)
		c.pid, c.ppid = 9, 8
		if c.exe == "" {
			c.exe = "/opt/python/bin/python3" // a cached environment's bin/python links to an interpreter outside
		}
		return append(outside(cache, lock), fakeProc{pid: 8, ppid: 1, argv: []string{"uv", "tool", "uvx", "srv"}, fds: []string{lock}}, c)
	}
}

func TestAUVPruneTakesTheLockWhenItsHoldersAllRunOutsideTheCache(t *testing.T) {
	f := newUVLock(t, outside, 4, 6)
	g := f.run(t, options(t, f.proc, time.Now(), nil))
	if !g.Done || !f.pruned(true) || f.pruned(false) {
		t.Fatalf("with every holder outside the cache, the prune runs with --force: %+v %v", g, f.ran)
	}
	if !*f.lockedAtPrune {
		t.Error("the prune runs under a lock in the lock file's place, which a new uv process waits for")
	}
	var st syscall.Stat_t
	must(t, syscall.Stat(f.lock, &st))
	if st.Ino == f.id.ino || st.Mode&0o777 != 0o666 {
		t.Errorf("the lock file in place is a new file with the old one's mode: inode %d (was %d), mode %o", st.Ino, f.id.ino, st.Mode&0o777)
	}
	fh, err := os.Open(f.lock)
	must(t, err)
	defer fh.Close()
	if err := syscall.Flock(int(fh.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Errorf("the lock is let go after the prune: %v", err)
	}
	if exists(f.lock + ".capacity") {
		t.Error("no file of capacity's is left beside the lock")
	}
}

func TestAUVPruneWaitsForWhatItWouldBreak(t *testing.T) {
	for _, c := range []struct {
		name    string
		procs   func(cache, lock string) []fakeProc
		holders []int
		after   func(*testing.T, *uvLockFixture)
		why     string
	}{
		{"a tool run from the cache, by its program", fromCache(func(cache string) fakeProc {
			py := filepath.Join(cache, "archive-v0", "x", "lib", "python3")
			return fakeProc{argv: []string{"python", "srv"}, exe: py}
		}), []int{4, 6, 8}, nil, "uv tool uvx srv (pid 8)"},
		{"a tool run from the cache, by the program it was started as", fromCache(func(cache string) fakeProc {
			return fakeProc{argv: []string{filepath.Join(cache, "archive-v0", "x", "bin", "python"), "srv"}}
		}), []int{4, 6, 8}, nil, "uv tool uvx srv (pid 8)"},
		{"a tool's command, by its PATH", fromCache(func(cache string) fakeProc {
			return fakeProc{argv: []string{"srv"}, env: []string{"PATH=" + filepath.Join(cache, "archive-v0", "y", "bin") + ":/usr/bin"}}
		}), []int{4, 6, 8}, nil, "uv tool uvx srv (pid 8)"},
		{"a script's command, by VIRTUAL_ENV", fromCache(func(cache string) fakeProc {
			return fakeProc{argv: []string{"python", "s.py"}, env: []string{"VIRTUAL_ENV=" + filepath.Join(cache, "environments-v2", "s-1")}}
		}), []int{4, 6, 8}, nil, "uv tool uvx srv (pid 8)"},
		{"a script whose environment is in the cache, by a mapped file", func(cache, lock string) []fakeProc {
			return append(outside(cache, lock), fakeProc{pid: 8, ppid: 1, argv: []string{"uv", "run", "--script", "s.py"}, fds: []string{lock}},
				fakeProc{pid: 9, ppid: 8, argv: []string{"python", "s.py"}, maps: []string{filepath.Join(cache, "environments-v2", "s-1", "lib", "x.so")}})
		}, []int{4, 6, 8}, nil, "uv run --script s.py (pid 8)"},
		{"a holder the look cannot read", outside, []int{4, 6, 4242}, nil, "the look cannot read"},
		{"a uv process that took the old lock file as the new one went in", outside, []int{4, 6},
			func(t *testing.T, f *uvLockFixture) { f.holds(t, 77) }, "took the cache's lock"},
		{"a process that came to use the cache meanwhile", outside, []int{4, 6}, func(t *testing.T, f *uvLockFixture) {
			dir := filepath.Join(f.proc, "12")
			must(t, os.MkdirAll(filepath.Join(dir, "fd"), 0o755))
			write(t, filepath.Join(dir, "cmdline"), "python3\x00")
			write(t, filepath.Join(dir, "stat"), "12 (python3) S 1 0 0 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 100 0 0\n")
			write(t, filepath.Join(dir, "maps"), "7f-7f r--s 0 0:0 1 "+filepath.Join(f.cache, "archive-v0", "y", "lib.so")+"\n")
		}, "python3 (pid 12)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newUVLock(t, c.procs, c.holders...)
			o := options(t, f.proc, time.Now(), nil)
			if c.after != nil {
				o.afterUVLock = func() { c.after(t, f) }
			}
			g := f.run(t, o)
			if g.Done || f.pruned(true) || f.pruned(false) || g.Action != ActReport || g.class != "busy" || !strings.Contains(g.Why, c.why) {
				t.Fatalf("no prune while it would break a process, which is named (%s): %+v %v", c.why, g, f.ran)
			}
			fh, err := os.Open(f.lock)
			must(t, err)
			defer fh.Close()
			if err := syscall.Flock(int(fh.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				t.Errorf("a lock capacity put in place is let go when it does not prune: %v", err)
			}
		})
	}
}

func TestWithoutTheLocksATakenLockIsUVsToDecide(t *testing.T) {
	f := newUVLock(t, outside, 4, 6)
	must(t, os.Remove(filepath.Join(f.proc, "locks"))) // which processes hold it cannot be read
	g := f.run(t, options(t, f.proc, time.Now(), nil))
	if !g.Done || !f.pruned(false) || f.pruned(true) {
		t.Fatalf("without the locks, the prune takes uv's own lock: %+v %v", g, f.ran)
	}
}
