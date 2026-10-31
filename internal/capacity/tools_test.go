package capacity

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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
		{"a uv run past its sync, outside the cache", []fakeProc{{pid: 4, ppid: 1, argv: []string{"uv", "run", "server"}, env: uvEnv},
			{pid: 5, ppid: 4, argv: []string{"python", "server.py"}, exe: "/proj/.venv/bin/python", env: started("/proj/.venv/bin")}},
			true, ActDelete},
		{"a uvx tool server runs from the cache", []fakeProc{{pid: 4, ppid: 1, argv: []string{"uvx", "mcp-x"}, env: uvEnv},
			{pid: 5, ppid: 4, argv: []string{"/home/u/.cache/uv/archive-v0/x/bin/python", "mcp-x"},
				exe: "/home/u/.cache/uv/archive-v0/x/bin/python", env: started("/home/u/.cache/uv/archive-v0/x/bin")}}, false, ActReport},
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
// that checks, as it prunes, whether the file in the lock file's place is held exclusively, and whether it was
// handed that file.
type uvLockFixture struct {
	cache, lock, proc string
	id                lockID
	ran               []string
	lockedAtPrune     *bool
	handedLock        *bool
}

// uvEnv is a uv process's environment; started is the one uv gives a command it starts: an environment's bin put
// first on uv's PATH.
var uvEnv = []string{"PATH=/usr/bin", "HOME=/home/u"}

func started(bin string, more ...string) []string {
	return append([]string{"PATH=" + bin + ":/usr/bin", "HOME=/home/u"}, more...)
}

// newUVLock makes the cache, then the processes procs gives for it, of which holders hold its lock.
func newUVLock(t *testing.T, procs func(cache, lock string) []fakeProc, holders ...int) *uvLockFixture {
	t.Helper()
	return newUVLockIn(t, filepath.Join(t.TempDir(), "uv"), procs, holders...)
}

// newUVLockIn is newUVLock with the cache at a path of the test's choosing.
func newUVLockIn(t *testing.T, cache string, procs func(cache, lock string) []fakeProc, holders ...int) *uvLockFixture {
	t.Helper()
	f := &uvLockFixture{cache: cache, lockedAtPrune: new(bool), handedLock: new(bool)}
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
	// a lock on another file, and a process waiting for it, are another file's
	fmt.Fprintf(&locks, "%d: FLOCK  ADVISORY  WRITE 555 %02x:%02x:%d 0 EOF\n", len(holders)+1, f.id.major, f.id.minor, f.id.ino+1)
	fmt.Fprintf(&locks, "%d: -> FLOCK  ADVISORY  WRITE 99 %02x:%02x:%d 0 EOF\n", len(holders)+1, f.id.major, f.id.minor, f.id.ino+1)
	write(t, filepath.Join(f.proc, "locks"), locks.String())
	return f
}

// addLock adds a line to the locks file for the lock file's first inode: a holder's ("FLOCK  ADVISORY  READ") or a
// waiter's ("-> FLOCK  ADVISORY  WRITE").
func (f *uvLockFixture) addLock(t *testing.T, pid int, lock string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.proc, "locks"))
	must(t, err)
	write(t, filepath.Join(f.proc, "locks"), string(b)+fmt.Sprintf("9: %s %d %02x:%02x:%d 0 EOF\n", lock, pid,
		f.id.major, f.id.minor, f.id.ino))
}

// holds adds a holder of the lock file's first inode; waits adds a process that waits for it.
func (f *uvLockFixture) holds(t *testing.T, pid int) {
	t.Helper()
	f.addLock(t, pid, "FLOCK  ADVISORY  READ")
}

func (f *uvLockFixture) waits(t *testing.T, pid int) {
	t.Helper()
	f.addLock(t, pid, "-> FLOCK  ADVISORY  WRITE")
}

// addProc adds a process to the fixture's /proc after it was made, as one that starts does.
func (f *uvLockFixture) addProc(t *testing.T, pid, ppid int, argv string, env string) string {
	t.Helper()
	dir := filepath.Join(f.proc, strconv.Itoa(pid))
	must(t, os.MkdirAll(filepath.Join(dir, "fd"), 0o755))
	write(t, filepath.Join(dir, "cmdline"), argv)
	write(t, filepath.Join(dir, "stat"), fmt.Sprintf("%d (x) S %d 0 0 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 300 0 0\n", pid, ppid))
	write(t, filepath.Join(dir, "maps"), "")
	if env != "" {
		write(t, filepath.Join(dir, "environ"), env)
	}
	return dir
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

func (u lockCheckingUV) UVWith(ctx context.Context, d time.Duration, files []*os.File, args ...string) (string, error) {
	for _, fh := range files {
		a, err1 := fh.Stat()
		b, err2 := os.Stat(u.f.lock)
		*u.f.handedLock = *u.f.handedLock || err1 == nil && err2 == nil && os.SameFile(a, b)
	}
	return u.UV(ctx, d, args...)
}

func (f *uvLockFixture) tools(answers map[string]string) lockCheckingUV {
	all := map[string]string{"uv cache dir": f.cache + "\n", "uv cache size": "40000000000\n", "docker": "ERR"}
	for k, v := range answers {
		all[k] = v
	}
	return lockCheckingUV{fakeTools{ran: &f.ran, answers: all}, f}
}

func (f *uvLockFixture) run(t *testing.T, o Options) Finding {
	t.Helper()
	o.Tools = f.tools(nil)
	return f.runWith(t, o, f.cache)
}

// runWith runs the uv rule with the options' tools, and gives its finding for the cache as uv names it.
func (f *uvLockFixture) runWith(t *testing.T, o Options, name string) Finding {
	t.Helper()
	o.Rules = []string{RuleUV}
	res, err := Run(context.Background(), Config{Live: []string{RuleUV}}.Normal(), openStore(t), o)
	if err != nil {
		t.Fatal(err)
	}
	return findings(res, RuleUV)[name]
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
	return []fakeProc{{pid: 4, ppid: 1, argv: []string{"uv", "tool", "run", "pkg"}, fds: []string{lock}, env: uvEnv},
		{pid: 5, ppid: 4, argv: []string{"/opt/tools/pkg/bin/python", "pkg"}, exe: "/opt/python/bin/python3",
			env: started("/opt/tools/pkg/bin")},
		{pid: 6, ppid: 1, argv: []string{"uv", "run", "server"}, fds: []string{lock}, env: uvEnv},
		{pid: 7, ppid: 6, argv: []string{"python", "server.py"}, exe: "/opt/python/bin/python3",
			env: started("/proj/.venv/bin", "VIRTUAL_ENV=/proj/.venv", "UV_RUN_RECURSION_DEPTH=1")}}
}

// fromCache is a uv process that holds the lock and a command of it that runs from the cache, as cmd says.
func fromCache(cmd func(cache string) fakeProc) func(cache, lock string) []fakeProc {
	return func(cache, lock string) []fakeProc {
		c := cmd(cache)
		c.pid, c.ppid = 9, 8
		if c.exe == "" {
			c.exe = "/opt/python/bin/python3" // a cached environment's bin/python links to an interpreter outside
		}
		if _, ok := envValue(c.env, "PATH"); !ok {
			c.env = append(c.env, started("/opt/srv/bin")...) // uv started it; only what the case gives is in the cache
		}
		return append(outside(cache, lock), fakeProc{pid: 8, ppid: 1, argv: []string{"uv", "tool", "uvx", "srv"},
			fds: []string{lock}, env: uvEnv}, c)
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
			return fakeProc{argv: []string{"srv"}, env: started(filepath.Join(cache, "archive-v0", "y", "bin"))}
		}), []int{4, 6, 8}, nil, "uv tool uvx srv (pid 8)"},
		{"a PATH that names a directory in the cache", func(cache, lock string) []fakeProc {
			path := "PATH=/usr/bin:" + filepath.Join(cache, "tools-v0", "x", "libexec") // uv's own PATH, which its command's ends with
			return append(outside(cache, lock), fakeProc{pid: 8, ppid: 1, argv: []string{"uv", "tool", "uvx", "srv"}, fds: []string{lock},
				env: []string{path}}, fakeProc{pid: 9, ppid: 8, argv: []string{"srv"}, env: []string{"PATH=/opt/srv/bin:" + path[5:]}})
		}, []int{4, 6, 8}, nil, "uv tool uvx srv (pid 8)"},
		{"a script's command, by VIRTUAL_ENV", fromCache(func(cache string) fakeProc {
			return fakeProc{argv: []string{"python", "s.py"}, env: []string{"VIRTUAL_ENV=" + filepath.Join(cache, "environments-v2", "s-1")}}
		}), []int{4, 6, 8}, nil, "uv tool uvx srv (pid 8)"},
		{"a script whose environment is in the cache, by a mapped file", func(cache, lock string) []fakeProc {
			return append(outside(cache, lock), fakeProc{pid: 8, ppid: 1, argv: []string{"uv", "run", "--script", "s.py"},
				fds: []string{lock}, env: uvEnv}, fakeProc{pid: 9, ppid: 8, argv: []string{"python", "s.py"},
				maps: []string{filepath.Join(cache, "environments-v2", "s-1", "lib", "x.so")},
				env:  started("/opt/s/bin", "UV_RUN_RECURSION_DEPTH=1")})
		}, []int{4, 6, 8}, nil, "uv run --script s.py (pid 8)"},
		{"a uv process that took the old lock file as the new one went in", outside, []int{4, 6}, func(t *testing.T, f *uvLockFixture) {
			f.addProc(t, 77, 1, "uv\x00run\x00x\x00", "")
			f.holds(t, 77)
		}, "took the cache's lock"},
		{"a clean that came to wait for the lock as the new one went in", outside, []int{4, 6},
			func(t *testing.T, f *uvLockFixture) { f.waits(t, 98) }, "waits for the cache's lock"},
		{"a process that came to use the cache meanwhile", outside, []int{4, 6}, func(t *testing.T, f *uvLockFixture) {
			dir := f.addProc(t, 12, 1, "python3\x00", "")
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

func TestAHolderTheLookCannotReadIsNamedAndOneThatEndedIsNot(t *testing.T) {
	f := newUVLock(t, outside, 4, 6, 4242, 4243)
	must(t, os.MkdirAll(filepath.Join(f.proc, "4242"), 0o755)) // it runs; its command line cannot be read
	g := f.run(t, options(t, f.proc, time.Now(), nil))
	if f.pruned(true) || g.Done || !strings.Contains(g.Why, "the look cannot read holds the lock (pid 4242)") || strings.Contains(g.Why, "4243") {
		t.Fatalf("a holder the look cannot read is named, and one that ended since the locks were read is not: %s %q %v", g.Action, g.Why, f.ran)
	}
	f = newUVLock(t, outside, 4, 6, 4243)
	if g := f.run(t, options(t, f.proc, time.Now(), nil)); !g.Done || !f.pruned(true) {
		t.Fatalf("a holder that ended holds the lock no more: %s %q %v", g.Action, g.Why, f.ran)
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

func TestAUVXThatStillInstallsIsNotPrunedUnder(t *testing.T) {
	for name, argv := range map[string][]string{
		"uvx":                              {"/home/u/.local/bin/uv", "tool", "uvx", "--python", "3.12", "srv@latest"},
		"global option":                    {"uv", "--directory", "/srv/mcp", "run", "server.py"},
		"versioned":                        {"/opt/uv@0.13.5", "tool", "run", "srv"},
		"versioned uvx":                    {"/opt/uvx@0.13.5", "srv"},
		"an install":                       {"uv", "--offline", "tool", "install", "srv"},
		"uv's own clean":                   {"uv", "cache", "clean"},
		"a command the rule does not know": {"uv", "tree"},
	} {
		t.Run(name, func(t *testing.T) {
			procs := func(cache, lock string) []fakeProc {
				return append(outside(cache, lock), fakeProc{pid: 8, ppid: 1, argv: argv, fds: []string{lock}, env: uvEnv})
			}
			f := newUVLock(t, procs, 4, 6, 8)
			g := f.run(t, options(t, f.proc, time.Now(), nil))
			if f.pruned(true) || f.pruned(false) || g.Done || g.Action != ActKeep || !strings.Contains(g.Why, "waits for an install") {
				t.Fatalf("a uv process that may write the cache now is waited for quietly: %s %q %v", g.Action, g.Why, f.ran)
			}
			if id, err := fileID(f.lock); err != nil || id != f.id {
				t.Errorf("capacity's lock never takes the place of one such a process holds: %v %v", id, err)
			}
		})
	}
}

func TestAHelperUVRunsIsNotTheCommandItStarts(t *testing.T) {
	for _, c := range []struct {
		name          string
		uv, helper    []string
		uvEnv, helped []string
	}{
		// a helper gets uv's environment, and the UV uv sets for its children
		{"the libc probe", []string{"/home/u/.local/bin/uv", "tool", "uvx", "srv"}, []string{"/lib64/ld-linux-x86-64.so.2", "--version"},
			uvEnv, append([]string{"UV=/home/u/.local/bin/uv"}, uvEnv...)},
		{"nvidia-smi", []string{"uv", "tool", "uvx", "--torch-backend", "auto", "srv"}, []string{"nvidia-smi", "-q"},
			uvEnv, append([]string{"UV=/home/u/.local/bin/uv"}, uvEnv...)},
		{"a keyring provider", []string{"uv", "tool", "run", "--keyring-provider", "subprocess", "srv"},
			[]string{"keyring", "get", "https://pypi.example/simple", "u"}, uvEnv, uvEnv},
		{"a helper of a uv run inside another", []string{"uv", "run", "srv"}, []string{"git", "lfs", "version"},
			append([]string{"UV_RUN_RECURSION_DEPTH=1"}, uvEnv...), append([]string{"UV_RUN_RECURSION_DEPTH=1"}, uvEnv...)},
		{"a helper given another PATH", []string{"uv", "tool", "uvx", "srv"}, []string{"helper"}, uvEnv,
			[]string{"PATH=/opt/x/libexec:/usr/bin"}},
		{"a uv process that ended as the look read it", []string{"uv", "tool", "uvx", "srv"}, []string{"srv"}, nil,
			started("/opt/srv/bin")},
	} {
		t.Run(c.name, func(t *testing.T) {
			procs := func(cache, lock string) []fakeProc {
				return append(outside(cache, lock), fakeProc{pid: 8, ppid: 1, argv: c.uv, fds: []string{lock}, env: c.uvEnv},
					fakeProc{pid: 9, ppid: 8, argv: c.helper, env: c.helped})
			}
			f := newUVLock(t, procs, 4, 6, 8)
			g := f.run(t, options(t, f.proc, time.Now(), nil))
			if f.pruned(true) || f.pruned(false) || g.Done || g.Action != ActKeep || !strings.Contains(g.Why, argvHead(c.uv)+" (pid 8)") {
				t.Fatalf("a uv process whose child has uv's own environment has not started its command, and is waited for quietly: %s %q %v",
					g.Action, g.Why, f.ran)
			}
			if id, err := fileID(f.lock); err != nil || id != f.id {
				t.Errorf("capacity's lock never takes the place of one such a process holds: %v %v", id, err)
			}
		})
	}
}

func TestAHolderWhoseCommandIsNotTheOneTheLookSawIsWaitedFor(t *testing.T) {
	for name, after := range map[string]func(*testing.T, *uvLockFixture){
		"its command ended": func(t *testing.T, f *uvLockFixture) { must(t, os.RemoveAll(filepath.Join(f.proc, "5"))) },
		"another runs": func(t *testing.T, f *uvLockFixture) {
			must(t, os.RemoveAll(filepath.Join(f.proc, "5")))
			f.addProc(t, 15, 4, "python\x00pkg\x00", "PATH=/opt/tools/pkg/bin:/usr/bin\x00")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newUVLock(t, outside, 4, 6)
			o := options(t, f.proc, time.Now(), nil)
			o.afterUVLock = func() { after(t, f) }
			g := f.run(t, o)
			if f.pruned(true) || g.Done || g.Action != ActKeep || !strings.Contains(g.Why, "uv tool run pkg (pid 4), whose command is not the one the look saw") {
				t.Fatalf("a holder that does not run the command the look saw may be at its sync: %s %q %v", g.Action, g.Why, f.ran)
			}
		})
	}
}

func TestTheLookUnderTheLockNamesAProcessUnderAHolderItCannotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every process")
	}
	f := newUVLock(t, outside, 4, 6)
	o := options(t, f.proc, time.Now(), nil)
	o.afterUVLock = func() { // the installed tool's command ran a program under sudo
		dir := f.addProc(t, 16, 5, "sudo\x00helper\x00", "")
		must(t, os.Chmod(filepath.Join(dir, "fd"), 0))
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(f.proc, "16", "fd"), 0o755) })
	g := f.run(t, o)
	if f.pruned(true) || g.Done || !strings.Contains(g.Why, "cannot read runs under a holder of the lock (pid 16)") {
		t.Fatalf("the look under capacity's lock names a process under a holder that it cannot read: %s %q %v", g.Action, g.Why, f.ran)
	}
}

func TestAHolderWhoseFilesCannotBeReadIsWaitedFor(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every process")
	}
	procs := func(cache, lock string) []fakeProc {
		return append(outside(cache, lock),
			fakeProc{pid: 8, ppid: 1, argv: []string{"uv", "run", "--with", "x", "srv"}, fds: []string{lock}, env: uvEnv},
			fakeProc{pid: 9, ppid: 8, argv: []string{"python", "srv"},
				maps: []string{filepath.Join(cache, "archive-v0", "x", "lib.so")},
				env:  started(filepath.Join(cache, "builds-v0", ".tmpA", "bin"))})
	}
	f := newUVLock(t, procs, 4, 6, 8)
	for _, pid := range []string{"8", "9"} {
		dir := filepath.Join(f.proc, pid)
		for _, p := range []string{"fd", "maps", "environ"} {
			must(t, os.Chmod(filepath.Join(dir, p), 0))
		}
		t.Cleanup(func() {
			os.Chmod(filepath.Join(dir, "fd"), 0o755)
			os.Chmod(filepath.Join(dir, "maps"), 0o644)
			os.Chmod(filepath.Join(dir, "environ"), 0o644)
		})
	}
	g := f.run(t, options(t, f.proc, time.Now(), nil))
	if f.pruned(true) || g.Done || !strings.Contains(g.Why, "cannot read") {
		t.Fatalf("a holder whose files and environment cannot be read is one the look cannot read: %s %q %v", g.Action, g.Why, f.ran)
	}
}

func TestAProcessUnderAHolderThatTheRunCannotReadIsNamed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every process")
	}
	procs := func(cache, lock string) []fakeProc {
		return append(outside(cache, lock), fakeProc{pid: 8, ppid: 1, argv: []string{"uv", "run", "srv"}, fds: []string{lock}, env: uvEnv},
			fakeProc{pid: 9, ppid: 8, argv: []string{"srv"}, env: started("/opt/srv/bin")},
			fakeProc{pid: 10, ppid: 9, argv: []string{"sudo", "helper"}, env: started("/opt/srv/bin")})
	}
	f := newUVLock(t, procs, 4, 6, 8)
	must(t, os.Chmod(filepath.Join(f.proc, "10", "fd"), 0))
	t.Cleanup(func() { os.Chmod(filepath.Join(f.proc, "10", "fd"), 0o755) })
	g := f.run(t, options(t, f.proc, time.Now(), nil))
	if f.pruned(true) || g.Done || !strings.Contains(g.Why, "cannot read runs under a holder of the lock (pid 10)") {
		t.Fatalf("a process under a holder's command whose open files the run may not read is unseen: %s %q %v", g.Action, g.Why, f.ran)
	}
}

func TestADryRunSaysAWaitingCleanKeepsThePrune(t *testing.T) {
	f := newUVLock(t, outside, 4, 6)
	f.waits(t, 99)
	o := options(t, f.proc, time.Now(), nil)
	o.DryRun = true
	if g := f.run(t, o); g.Action != ActReport || !strings.Contains(g.Why, "waits for the cache's lock") {
		t.Errorf("a dry run says what the run would do: wait for the clean: %s %q", g.Action, g.Why)
	}
}

func TestACleanWaitingAtTheSwapKeepsTheLockFile(t *testing.T) {
	f := newUVLock(t, outside, 4, 6)
	f.waits(t, 99)
	holder, err := os.OpenFile(f.lock, os.O_RDWR, 0)
	must(t, err)
	defer holder.Close()
	must(t, syscall.Flock(int(holder.Fd()), syscall.LOCK_SH)) // a project's uv run
	clean, err := os.OpenFile(f.lock, os.O_RDWR, 0)
	must(t, err)
	got := make(chan struct{})
	go func() { syscall.Flock(int(clean.Fd()), syscall.LOCK_EX); close(got) }()
	time.Sleep(100 * time.Millisecond) // it waits behind the holder
	g := f.run(t, options(t, f.proc, time.Now(), nil))
	if g.Done || f.pruned(true) || !strings.Contains(g.Why, "waits for the cache's lock") {
		t.Errorf("a waiter on the lock keeps capacity's lock out of its place: %s %q", g.Action, g.Why)
	}
	if id, err := fileID(f.lock); err != nil || id != f.id {
		t.Errorf("the lock file the clean waits on stays the one every uv process opens: %v %v", id, err)
	}
	holder.Close() // the project's run ends, and the clean gets the lock everyone shares
	<-got
	clean.Close()
}

// dockerHook runs hook at the uv rule's first `docker ps`: after its first read of /proc/locks, before the rename.
type dockerHook struct {
	lockCheckingUV
	armed *bool
	hook  func()
}

func (d dockerHook) UV(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	if strings.Join(args, " ") == "cache size --preview-features cache-size" {
		*d.armed = true
	}
	return d.lockCheckingUV.UV(ctx, timeout, args...)
}

func (d dockerHook) Docker(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	if *d.armed && len(args) > 0 && args[0] == "ps" {
		*d.armed = false
		d.hook()
	}
	return d.lockCheckingUV.Docker(ctx, timeout, args...)
}

func TestAWaiterThatCameBeforeTheRenameKeepsTheLockFileInPlace(t *testing.T) {
	f := newUVLock(t, outside, 4, 6)
	holder, err := os.OpenFile(f.lock, os.O_RDWR, 0)
	must(t, err)
	defer holder.Close()
	must(t, syscall.Flock(int(holder.Fd()), syscall.LOCK_SH)) // the project's uv run
	var clean *os.File
	got := make(chan struct{})
	armed := false
	o := options(t, f.proc, time.Now(), nil)
	o.Tools = dockerHook{f.tools(nil), &armed, func() { // a `uv cache clean` opens the lock file during the look, and waits
		f.waits(t, 98)
		clean, err = os.OpenFile(f.lock, os.O_RDWR, 0)
		must(t, err)
		go func() { syscall.Flock(int(clean.Fd()), syscall.LOCK_EX); close(got) }()
		time.Sleep(50 * time.Millisecond)
	}}
	g := f.runWith(t, o, f.cache)
	if g.Done || f.pruned(true) || !strings.Contains(g.Why, "waits for the cache's lock") {
		t.Errorf("a clean that came to wait during the look keeps capacity's lock out of its place: %s %q", g.Action, g.Why)
	}
	later, err := os.OpenFile(f.lock, os.O_RDWR, 0) // a uvx that starts after the run, from a cached environment
	must(t, err)
	if err := syscall.Flock(int(later.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		t.Fatalf("a new uv process takes uv's lock shared: %v", err)
	}
	holder.Close() // the project's run ends
	select {
	case <-got:
		t.Errorf("the clean holds uv's lock alone while a later uv process holds the lock file shared (%s %q)", g.Action, g.Why)
	case <-time.After(500 * time.Millisecond):
	}
	later.Close()
	<-got
	clean.Close()
}

func TestNoSwapUnderUVsOwnClean(t *testing.T) {
	for _, argv := range [][]string{{"uv", "cache", "clean"}, {"uv", "clean"}} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			procs := func(cache, lock string) []fakeProc {
				return []fakeProc{{pid: 8, ppid: 1, argv: argv, fds: []string{lock}, env: uvEnv},
					{pid: 9, ppid: 8, argv: []string{"helper"}}} // a child, so only the lock's kind tells
			}
			f := newUVLock(t, procs)
			write(t, filepath.Join(f.proc, "locks"), fmt.Sprintf("1: FLOCK  ADVISORY  WRITE 8 %02x:%02x:%d 0 EOF\n",
				f.id.major, f.id.minor, f.id.ino))
			g := f.run(t, options(t, f.proc, time.Now(), nil))
			if f.pruned(true) || g.Done || g.Action != ActKeep || !strings.Contains(g.Why, strings.Join(argv, " ")+" (pid 8)") {
				t.Fatalf("uv's own clean holds the lock exclusively, and is waited for quietly: %s %q %v", g.Action, g.Why, f.ran)
			}
			l := lockHolders(f.proc, []lockID{f.id})
			if len(l.writers) == 0 || !strings.Contains(l.blocks(), "alone") {
				t.Errorf("a WRITE lock is one held alone: %+v", l)
			}
		})
	}
}

func TestAnEnvironmentLinkedIntoTheCacheIsUsed(t *testing.T) {
	for name, env := range map[string]func(t *testing.T, base, cache string) []string{
		"packages linked into the cache": func(t *testing.T, base, cache string) []string {
			site := filepath.Join(base, "proj", ".venv", "lib", "python3.12", "site-packages")
			target := filepath.Join(cache, "archive-v0", "w", "pkg")
			must(t, os.MkdirAll(target, 0o755))
			must(t, os.MkdirAll(site, 0o755))
			must(t, os.Symlink(target, filepath.Join(site, "pkg")))
			return started(filepath.Join(base, "proj", ".venv", "bin"), "VIRTUAL_ENV="+filepath.Join(base, "proj", ".venv"))
		},
		"packages as uv's symlink link mode lays them out": func(t *testing.T, base, cache string) []string {
			// uv makes a package's directories and links its files
			site := filepath.Join(base, "proj", ".venv", "lib", "python3.12", "site-packages")
			arch := filepath.Join(cache, "archive-v0", "w")
			write(t, filepath.Join(arch, "pkg", "__init__.py"), "")
			must(t, os.MkdirAll(filepath.Join(site, "pkg"), 0o755))
			must(t, os.Symlink(filepath.Join(arch, "pkg", "__init__.py"), filepath.Join(site, "pkg", "__init__.py")))
			return started(filepath.Join(base, "proj", ".venv", "bin"), "VIRTUAL_ENV="+filepath.Join(base, "proj", ".venv"))
		},
		"an environment that is a link into the cache": func(t *testing.T, base, cache string) []string {
			target := filepath.Join(cache, "environments-v2", "proj-1")
			must(t, os.MkdirAll(filepath.Join(target, "bin"), 0o755))
			must(t, os.MkdirAll(filepath.Join(base, "proj2"), 0o755))
			must(t, os.Symlink(target, filepath.Join(base, "proj2", ".venv")))
			return started(filepath.Join(base, "proj2", ".venv", "bin"))
		},
		"uv's symlink link mode": func(t *testing.T, base, cache string) []string {
			return started("/proj/.venv/bin", "UV_LINK_MODE=symlink")
		},
	} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			procs := func(cache, lock string) []fakeProc {
				return append(outside(cache, lock), fakeProc{pid: 8, ppid: 1, argv: []string{"uv", "run", "srv"}, fds: []string{lock}, env: uvEnv},
					fakeProc{pid: 9, ppid: 8, argv: []string{"python", "srv"}, env: env(t, base, cache)})
			}
			f := newUVLock(t, procs, 4, 6, 8)
			g := f.run(t, options(t, f.proc, time.Now(), nil))
			if f.pruned(true) || g.Done || !strings.Contains(g.Why, "uv run srv (pid 8)") {
				t.Fatalf("a process whose files are links into the cache uses it: %s %q %v", g.Action, g.Why, f.ran)
			}
		})
	}
}

func TestACacheBehindALinkIsSeenWhereTheKernelShowsIt(t *testing.T) {
	var real string
	procs := func(cache, lock string) []fakeProc {
		real = cache
		return append(outside(cache, lock), fakeProc{pid: 8, ppid: 1, argv: []string{"uv", "tool", "uvx", "srv"}, fds: []string{lock}, env: uvEnv},
			fakeProc{pid: 9, ppid: 8, argv: []string{"srv"}, maps: []string{filepath.Join(cache, "archive-v0", "x", "lib.so")},
				env: started("/opt/srv/bin")})
	}
	f := newUVLock(t, procs, 4, 6, 8)
	link := filepath.Join(t.TempDir(), "uv-link") // uv names the cache through a link; /proc shows where it is
	must(t, os.Symlink(real, link))
	o := options(t, f.proc, time.Now(), nil)
	o.Tools = f.tools(map[string]string{"uv cache dir": link + "\n"})
	g := f.runWith(t, o, link)
	if f.pruned(true) || g.Done || !strings.Contains(g.Why, "uv tool uvx srv (pid 8)") {
		t.Fatalf("a mapped file of the cache, seen by its real path, is a use: %s %q %v", g.Action, g.Why, f.ran)
	}
}

func TestACacheReachedThroughALinkPrunesPastHoldersOutsideIt(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	f := newUVLockIn(t, filepath.Join(dir, "uv"), outside, 4, 6) // each holder's lock as /proc shows it, where it is
	link := filepath.Join(t.TempDir(), "uv-link")
	must(t, os.Symlink(f.cache, link))
	o := options(t, f.proc, time.Now(), nil)
	o.Tools = f.tools(map[string]string{"uv cache dir": link + "\n"})
	if g := f.runWith(t, o, link); !g.Done || !f.pruned(true) {
		t.Fatalf("holders outside a cache that uv names through a link hold its lock and use nothing in it: %s %q %v", g.Action, g.Why, f.ran)
	}
}

func TestAContainerThatMountsWhatHoldsTheCacheIsWaitedFor(t *testing.T) {
	f := newUVLock(t, outside, 4, 6)
	o := options(t, f.proc, time.Now(), nil)
	o.Tools = f.tools(map[string]string{"docker ps": "c1\n", "docker inspect": "/box\t" + filepath.Dir(f.cache) + "\n",
		"docker info": "ERR"})
	if g := f.runWith(t, o, f.cache); f.pruned(true) || f.pruned(false) || !strings.Contains(g.Why, "container box mounts") {
		t.Fatalf("a container that mounts what holds the cache may run from it: %s %q %v", g.Action, g.Why, f.ran)
	}
}

func TestAProcessThatOpenedTheOldLockBeforeTheSwapKeepsIt(t *testing.T) {
	procs := func(cache, lock string) []fakeProc { // 13 opened the lock file and locks it once the new file is in place
		return append(outside(cache, lock), fakeProc{pid: 13, ppid: 1, argv: []string{"uv", "tree"}, env: uvEnv,
			fds: []string{lock + " (deleted)"}})
	}
	f := newUVLock(t, procs, 4, 6)
	must(t, os.Link(f.lock, f.lock+" (deleted)")) // the old file, where the fixture's /proc names it
	g := f.run(t, options(t, f.proc, time.Now(), nil))
	if f.pruned(true) || g.Done || !strings.Contains(g.Why, "opened the cache's lock as capacity put its own in place") {
		t.Fatalf("a uv process that opened the lock file before the swap, and locks it after, keeps it: %s %q %v", g.Action, g.Why, f.ran)
	}
}

func TestAProcessThatHasTheLockOpenAtTheSwapKeepsTheLockFileInPlace(t *testing.T) {
	for _, throughLink := range []bool{false, true} {
		t.Run(fmt.Sprintf("uv names the cache through a link %v", throughLink), func(t *testing.T) {
			dir, err := filepath.EvalSymlinks(t.TempDir())
			must(t, err)
			procs := func(cache, lock string) []fakeProc { // 13 opened the lock file and has not locked it yet
				return append(outside(cache, lock), fakeProc{pid: 13, ppid: 1, argv: []string{"uv", "tree"}, env: uvEnv, fds: []string{lock}})
			}
			f := newUVLockIn(t, filepath.Join(dir, "uv"), procs, 4, 6)
			name := f.cache
			o := options(t, f.proc, time.Now(), nil)
			if throughLink { // /proc shows the lock file where it is
				name = filepath.Join(t.TempDir(), "uv-link")
				must(t, os.Symlink(f.cache, name))
			}
			o.Tools = f.tools(map[string]string{"uv cache dir": name + "\n"})
			g := f.runWith(t, o, name)
			if f.pruned(true) || g.Done || !strings.Contains(g.Why, "opened the cache's lock since the look") {
				t.Fatalf("a process that has the lock file open, and is no holder, keeps it in place: %s %q %v", g.Action, g.Why, f.ran)
			}
			if id, err := fileID(f.lock); err != nil || id != f.id || exists(f.lock+".capacity") {
				t.Errorf("the lock file stays the one it opened, and nothing of capacity's is beside it: %v %v", id, err)
			}
		})
	}
}

func TestAProcessThatHoldsALockFileReplacedBeforeIsNoReasonToWait(t *testing.T) {
	procs := func(cache, lock string) []fakeProc { // a long-lived tool server holds the lock file an earlier prune replaced
		return append(outside(cache, lock), fakeProc{pid: 40, ppid: 1, argv: []string{"server"}, fds: []string{lock + " (deleted)"}})
	}
	f := newUVLock(t, procs, 4, 6)
	write(t, f.lock+" (deleted)", "") // that older file, where the fixture's /proc names it
	if g := f.run(t, options(t, f.proc, time.Now(), nil)); !g.Done || !f.pruned(true) {
		t.Fatalf("a lock file replaced before is not the one the swap replaces: %s %q %v", g.Action, g.Why, f.ran)
	}
}

func TestAUVProcessTheRunCannotReadThatRanAtTheLookDoesNotKeepTheSwap(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every process")
	}
	procs := func(cache, lock string) []fakeProc { // a container's uv, root's, which runs with a cache of its own
		return append(outside(cache, lock), fakeProc{pid: 30, ppid: 29, argv: []string{"uv", "run", "app"}, env: uvEnv},
			fakeProc{pid: 31, ppid: 30, argv: []string{"python", "app"}, env: started("/app/.venv/bin")})
	}
	f := newUVLock(t, procs, 4, 6)
	for _, p := range []string{"30/fd", "30/environ", "31/fd", "31/environ"} {
		must(t, os.Chmod(filepath.Join(f.proc, p), 0))
	}
	t.Cleanup(func() {
		for _, p := range []string{"30/fd", "31/fd"} {
			os.Chmod(filepath.Join(f.proc, p), 0o755)
		}
	})
	if g := f.run(t, options(t, f.proc, time.Now(), nil)); !g.Done || !f.pruned(true) {
		t.Fatalf("a uv process that ran at the look, holds no lock and cannot be read keeps nothing waiting: %s %q %v", g.Action, g.Why, f.ran)
	}
}

func TestAUVProcessWhoseEnvironmentCannotBeReadIsNamed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every process")
	}
	procs := func(cache, lock string) []fakeProc {
		return append(outside(cache, lock), fakeProc{pid: 8, ppid: 1, argv: []string{"uv", "run", "srv"}, fds: []string{lock}, env: uvEnv},
			fakeProc{pid: 9, ppid: 8, argv: []string{"srv"}, env: []string{"PATH=/opt/srv/bin:/usr/sbin"}})
	}
	f := newUVLock(t, procs, 4, 6, 8)
	must(t, os.Chmod(filepath.Join(f.proc, "8", "environ"), 0))
	g := f.run(t, options(t, f.proc, time.Now(), nil))
	if f.pruned(true) || g.Done || g.Action != ActReport || !strings.Contains(g.Why, "cannot read runs under a holder of the lock (pid 8)") {
		t.Fatalf("a holder whose environment cannot be read may run anything, and is named: %s %q %v", g.Action, g.Why, f.ran)
	}
}

func TestAnInstallThatHoldsNoLockYetIsWaitedFor(t *testing.T) {
	for _, argv := range [][]string{{"uv", "v"}, {"uv", "virtualenv", ".venv"}, {"uv", "upgrade"}, {"uv", "clean"},
		{"uv", "tool", "update", "x"}, {"uv", "python", "upgrade"}, {"uv", "cache", "clear"}} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			procs := func(cache, lock string) []fakeProc {
				return append(outside(cache, lock), fakeProc{pid: 8, ppid: 1, argv: argv, env: uvEnv})
			}
			f := newUVLock(t, procs, 4, 6)
			g := f.run(t, options(t, f.proc, time.Now(), nil))
			if f.pruned(true) || g.Done || g.Action != ActKeep || !strings.Contains(g.Why, strings.Join(argv, " ")+" (pid 8)") {
				t.Fatalf("a uv process that may write the cache is waited for, before it takes the lock too: %s %q %v", g.Action, g.Why, f.ran)
			}
		})
	}
}

func TestAHolderThatEndedUnderTheLockIsNoReasonToWait(t *testing.T) {
	f := newUVLock(t, outside, 4, 6)
	o := options(t, f.proc, time.Now(), nil)
	o.afterUVLock = func() { // the installed tool ended; /proc/locks still names the lock it held
		must(t, os.RemoveAll(filepath.Join(f.proc, "4")))
		must(t, os.RemoveAll(filepath.Join(f.proc, "5")))
	}
	if g := f.run(t, o); !g.Done || !f.pruned(true) {
		t.Fatalf("a holder that ended holds the old file no more: %s %q %v", g.Action, g.Why, f.ran)
	}
}

// recordResolved records the places the rule resolves an environment's links in while on is set.
func recordResolved(t *testing.T, on *bool) *[]string {
	t.Helper()
	var got []string
	was := resolveLinks
	t.Cleanup(func() { resolveLinks = was })
	resolveLinks = func(p string) (string, error) {
		if *on {
			got = append(got, p)
		}
		return was(p)
	}
	return &got
}

func TestTheLookUnderTheLockReadsNoFilesystemForAPlaceItDidNotAnswer(t *testing.T) {
	f := newUVLock(t, outside, 4, 6)
	on := false
	resolved := recordResolved(t, &on)
	o := options(t, f.proc, time.Now(), nil)
	o.afterUVLock = func() { // the project's command started a shell in another environment
		f.addProc(t, 17, 7, "sh\x00", "VIRTUAL_ENV=/opt/env2\x00PATH=/opt/env2/bin:/usr/bin\x00")
		on = true
	}
	if g := f.run(t, o); !g.Done || len(*resolved) > 0 {
		t.Fatalf("the look under capacity's lock reads /proc, and what the first look read: it resolved %v (%s %q)", *resolved, g.Action, g.Why)
	}
}

func TestOnlyAnEnvironmentsBinOnPATHIsLookedInto(t *testing.T) {
	procs := func(cache, lock string) []fakeProc {
		return append(outside(cache, lock), fakeProc{pid: 18, ppid: 1, argv: []string{"sh"}, env: []string{"PATH=/opt/a/sbin:/opt/b/bin"}})
	}
	f := newUVLock(t, procs, 4, 6)
	on := true
	resolved := recordResolved(t, &on)
	f.run(t, options(t, f.proc, time.Now(), nil))
	if slices.Contains(*resolved, "/opt/a") || !slices.Contains(*resolved, "/opt/b") {
		t.Errorf("an environment is looked for above a bin on PATH, and nowhere else: %v", *resolved)
	}
}

func TestAUVProcessTheRunCannotReadThatStartsAtTheSwapKeepsIt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every process")
	}
	f := newUVLock(t, outside, 4, 6)
	o := options(t, f.proc, time.Now(), nil)
	o.afterUVLock = func() { // a uv process under sudo started: the run may not read its open files
		must(t, os.Chmod(filepath.Join(f.addProc(t, 30, 29, "uv\x00cache\x00clean\x00", ""), "fd"), 0))
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(f.proc, "30", "fd"), 0o755) })
	g := f.run(t, o)
	if f.pruned(true) || g.Done || !strings.Contains(g.Why, "uv process (pid 30) that started as capacity put its own in place may have the cache's lock open") {
		t.Fatalf("a uv process the look did not see, whose open files cannot be read, may have opened the old lock file: %s %q %v", g.Action, g.Why, f.ran)
	}
}

func TestTheLookForOpenersStatsOnlyWhatNamesTheLock(t *testing.T) {
	var other string
	f := newUVLock(t, func(cache, lock string) []fakeProc {
		other = filepath.Join(filepath.Dir(cache), "mnt", "f") // a file on a filesystem that may not answer
		return []fakeProc{{pid: 20, ppid: 1, argv: []string{"server"}, fds: []string{other, lock + " (deleted)", lock}}}
	}, 4)
	write(t, other, "")
	defer func(s func(string) (os.FileInfo, error)) { statLock = s }(statLock)
	var statted []string
	statLock = func(p string) (os.FileInfo, error) {
		statted = append(statted, p)
		return os.Stat(p)
	}
	c := uvCache{canon: f.cache, real: f.cache, lock: f.lock, lockReal: f.lock}
	fd := func(n int) string { return filepath.Join(f.proc, "20", "fd", strconv.Itoa(n)) }
	for renamed, want := range map[bool]string{false: fd(5), true: fd(4)} {
		statted = nil
		openers(f.proc, f.id, c.isLock, renamed, &Sample{Table: map[int]Proc{}})
		if len(statted) != 1 || statted[0] != want {
			t.Errorf("only a link /proc names as the lock file is stat'ed (renamed %v): %v, want %s", renamed, statted, want)
		}
	}
}

func TestTheLocksAreReadByTheFilesystemsDeviceToo(t *testing.T) {
	f := newUVLock(t, outside)
	// /proc/locks names the lock file by its filesystem's device, which on btrfs is not the one stat gives
	write(t, filepath.Join(f.proc, "locks"), fmt.Sprintf("1: FLOCK  ADVISORY  READ 4 00:2a:%d 0 EOF\n"+
		"2: FLOCK  ADVISORY  READ 6 00:2a:%d 0 EOF\n", f.id.ino, f.id.ino))
	o := options(t, f.proc, time.Now(), nil)
	write(t, o.MountInfo, "29 1 0:42 / / rw - btrfs /dev/nvme0n1p2 rw\n")
	if g := f.run(t, o); !g.Done || !f.pruned(true) {
		t.Fatalf("holders named by the filesystem's device hold the lock: %s %q %v", g.Action, g.Why, f.ran)
	}
}

func TestALockFileThatChangedSinceTheLookIsNotReplaced(t *testing.T) {
	f := newUVLock(t, outside, 4, 6)
	held, why := uvSwap{lk: uvLockState{file: f.lock, id: lockID{f.id.major, f.id.minor, f.id.ino + 7}}, procRoot: f.proc}.lockInPlace()
	if held != nil || !strings.Contains(why, "changed since the look") {
		t.Errorf("a lock file that is not the one the look saw is left as it is: %v %q", held, why)
	}
}

func TestThePrunesTimeIsInItsDetailWhetherItRanOrFailed(t *testing.T) {
	f := newUVLock(t, outside, 4, 6)
	if g := f.run(t, options(t, f.proc, time.Now(), nil)); !g.Done || !regexp.MustCompile(` in \d+s$`).MatchString(g.Detail) {
		t.Errorf("the prune's detail says how long it took, so a long one shows: %q", g.Detail)
	}
	f = newUVLock(t, outside, 4, 6)
	o := options(t, f.proc, time.Now(), nil)
	o.Tools = f.tools(map[string]string{"uv cache prune": "ERR"})
	if g := f.runWith(t, o, f.cache); g.Done || g.Err == "" || !regexp.MustCompile(`; it failed after \d+s$`).MatchString(g.Detail) {
		t.Errorf("a prune that failed says after how long: %q %q", g.Detail, g.Err)
	}
}

func TestTheForcedPruneIsHandedCapacitysLock(t *testing.T) {
	f := newUVLock(t, outside, 4, 6)
	if g := f.run(t, options(t, f.proc, time.Now(), nil)); !g.Done || !*f.handedLock {
		t.Fatalf("the forced prune holds the file that holds capacity's lock, so the lock lasts while it runs: %+v", g)
	}
}

func TestAToolIsHandedTheFilesItGets(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	tool := filepath.Join(dir, "uv")
	write(t, tool, "#!/bin/sh\nreadlink /proc/$$/fd/3\n")
	must(t, os.Chmod(tool, 0o755))
	lock := filepath.Join(dir, ".lock")
	write(t, lock, "")
	fh, err := os.Open(lock)
	must(t, err)
	defer fh.Close()
	out, err := execTools{uv: tool}.UVWith(context.Background(), 5*time.Second, []*os.File{fh}, "cache", "prune")
	if strings.TrimSpace(out) != lock || err != nil {
		t.Errorf("uv gets the file it is handed as its fd 3: %q (%v)", strings.TrimSpace(out), err)
	}
}

func TestAUVProcessPastItsSyncIsKnownHoweverItIsStarted(t *testing.T) {
	cmd := started("/opt/tools/srv/bin")
	for _, c := range []struct {
		name          string
		argv          []string
		uvEnv, cmdEnv []string
	}{
		{"a versioned uv", []string{"/opt/uv@0.13.5", "tool", "run", "srv"}, uvEnv, cmd},
		{"uvx", []string{"/opt/uvx", "srv"}, uvEnv, cmd},
		{"a versioned uvx", []string{"/opt/uvx@0.13.5", "srv"}, uvEnv, cmd},
		{"a uvx with a suffix", []string{"/opt/uvx-0.13", "srv"}, uvEnv, cmd},
		{"the uv a uvx with a suffix runs", []string{"/opt/uv-0.13", "tool", "uvx", "srv"}, uvEnv, cmd},
		{"uv tool uvx", []string{"uv", "tool", "uvx", "srv"}, uvEnv, cmd},
		{"a global option", []string{"uv", "--directory", "/srv/mcp", "run", "server.py"}, uvEnv, cmd},
		{"global flags only", []string{"uv", "-q", "--offline", "run", "server.py"}, uvEnv, cmd},
		{"a uv with no PATH", []string{"uv", "tool", "run", "srv"}, []string{"HOME=/home/u"}, []string{"PATH=/opt/tools/srv/bin"}},
		{"a uv run's command, by the depth it counts", []string{"uv", "run", "server.py"}, uvEnv,
			append([]string{"UV_RUN_RECURSION_DEPTH=1"}, uvEnv...)},
	} {
		t.Run(c.name, func(t *testing.T) {
			procs := func(cache, lock string) []fakeProc {
				return append(outside(cache, lock), fakeProc{pid: 8, ppid: 1, argv: c.argv, fds: []string{lock}, env: c.uvEnv},
					fakeProc{pid: 9, ppid: 8, argv: []string{"/opt/tools/srv/bin/python", "srv"}, env: c.cmdEnv})
			}
			f := newUVLock(t, procs, 4, 6, 8)
			if g := f.run(t, options(t, f.proc, time.Now(), nil)); !g.Done || !f.pruned(true) {
				t.Fatalf("a uv process that runs its command outside the cache is one a forced prune leaves alone: %s %q %v", g.Action, g.Why, f.ran)
			}
		})
	}
}

func TestUVsProgramsAreKnownByTheirNames(t *testing.T) {
	for name, want := range map[string]string{"uv": "uv", "/home/u/.local/bin/uv": "uv", "uvx": "uvx", "uv@0.13.5": "uv",
		"uvx@0.13.5": "uvx", "uvx-0.13": "uvx", "uv-0.13": "uv", "uv0.13": "uv", "uvicorn": "", "uv-build": "", "uvloop": "",
		"uvxyz": "", "python": ""} {
		if got := uvProgram(name); got != want {
			t.Errorf("%s runs %q, want %q", name, got, want)
		}
	}
}

func TestACacheBehindABindMountIsSeenInAProcesssEnvironment(t *testing.T) {
	base := t.TempDir()
	shown := filepath.Join(base, "home", ".cache", "uv") // as uv names it, through the bind mount of the data's /cache
	procs := func(cache, lock string) []fakeProc {
		return append(outside(cache, lock), fakeProc{pid: 8, ppid: 1, argv: []string{"uv", "tool", "run", "srv"}, fds: []string{lock}, env: uvEnv},
			fakeProc{pid: 9, ppid: 8, argv: []string{"python", "srv"}, env: started(filepath.Join(cache, "archive-v0", "x", "bin"))})
	}
	f := newUVLockIn(t, shown, procs, 4, 6, 8)
	o := options(t, f.proc, time.Now(), nil)
	write(t, o.MountInfo, "29 1 259:3 / / rw - ext4 /dev/sda2 rw\n40 29 252:0 / "+filepath.Join(base, "data")+
		" rw - ext4 /dev/mapper/data rw\n41 29 252:0 /cache "+filepath.Join(base, "home", ".cache")+" rw - ext4 /dev/mapper/data rw\n")
	if g := f.run(t, o); f.pruned(true) || g.Done || !strings.Contains(g.Why, "uv tool run srv (pid 8)") {
		t.Fatalf("a PATH that names the cache through its bind mount runs from it: %s %q %v", g.Action, g.Why, f.ran)
	}
}
