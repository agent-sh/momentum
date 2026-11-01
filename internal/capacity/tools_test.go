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
			{pid: 5, ppid: 4, argv: []string{"python", "server.py"}, exe: "/proj/.venv/bin/python", env: ran("/proj/.venv/bin")}},
			true, ActDelete},
		// a process that runs from the cache gets a prune around it (uvaround_test.go), under the cache's lock: this
		// cache has no lock file to take
		{"a uvx tool server runs from a cache with no lock file", []fakeProc{{pid: 4, ppid: 1, argv: []string{"uvx", "mcp-x"}, env: uvEnv},
			{pid: 5, ppid: 4, argv: []string{"/home/u/.cache/uv/archive-v0/x/bin/python", "mcp-x"},
				exe: "/home/u/.cache/uv/archive-v0/x/bin/python", env: started("/home/u/.cache/uv/archive-v0/x/bin")}}, false, ActReport},
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

// Every hold of the uv prune waits the rule's day before it is a problem note, whatever holds it: a uv process that
// takes the cache's lock during the prune is listed by the run at once, with no note. The note comes once the prune
// has been held off for a day, by any reason in turn, dated from the hold's first scan; the scan that prunes ends it,
// and the next hold waits its own day.
func TestAUVHoldIsAProblemOnlyOnceItHasLastedADay(t *testing.T) {
	ctx := context.Background()
	t0 := time.Now()
	cfg := Config{Live: []string{RuleUV}}.Normal()
	st := openStore(t)
	run := func(at time.Time, prune string, procs ...fakeProc) Finding {
		t.Helper()
		o := options(t, procRoot(t, procs...), at, nil)
		o.Tools = fakeTools{answers: map[string]string{"uv cache dir": "/home/u/.cache/uv\n", "uv cache size": "40000000000\n",
			"uv cache prune": prune, "docker": "ERR"}}
		o.Rules = []string{RuleUV}
		res, err := Run(ctx, cfg, st, o)
		must(t, err)
		return findings(res, RuleUV)["/home/u/.cache/uv"]
	}
	standing := func() (store.Note, bool) {
		t.Helper()
		n, ok, err := st.Note(ctx, "capacity:uv:busy")
		must(t, err)
		return n, ok && n.Done.IsZero()
	}
	if f := run(t0, "ERR-INUSE"); f.Action != ActReport || f.class != "busy" || !strings.Contains(f.Why, "took the cache's lock") {
		t.Fatalf("setup: a uv process that took the lock during the prune holds it off, and the run says why: %+v", f)
	}
	if n, ok := standing(); ok {
		t.Fatalf("a hold is no problem at its first scan: %+v", n)
	}
	run(t0.Add(23*time.Hour), "ERR-INUSE")
	if n, ok := standing(); ok {
		t.Fatalf("nor 23 hours on: %+v", n)
	}
	// a day on, a quiet wait holds it off: an install
	install := fakeProc{pid: 4, ppid: 1, argv: []string{"/usr/bin/uv", "pip", "install", "x"}}
	if f := run(t0.Add(24*time.Hour), "ERR-INUSE", install); f.Action != ActReport || !strings.Contains(f.Why, "pip install x (pid 4)") {
		t.Fatalf("a quiet wait is reported once the prune has been held off for a day: %+v", f)
	}
	if n, ok := standing(); !ok || n.At.UnixMilli() != t0.UnixMilli() || !strings.Contains(n.Detail, "pip install x (pid 4)") {
		t.Fatalf("a day of holds is a problem, dated from the hold's first scan, naming what holds it now: %+v %v", n, ok)
	}
	if f := run(t0.Add(25*time.Hour), "Removed 3 files (1.0GiB)\n"); !f.Done {
		t.Fatalf("setup: the prune runs: %+v", f)
	}
	if n, ok := standing(); ok {
		t.Fatalf("the scan that prunes ends the hold's note: %+v", n)
	}
	run(t0.Add(26*time.Hour), "ERR-INUSE")
	if n, ok := standing(); ok {
		t.Fatalf("the next hold waits its own day: %+v", n)
	}
	run(t0.Add(50*time.Hour), "ERR-INUSE")
	if n, ok := standing(); !ok || n.At.UnixMilli() != t0.Add(26*time.Hour).UnixMilli() {
		t.Fatalf("and is a problem once it has waited it: %+v %v", n, ok)
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
		title          string // the run's note's title: what Docker says it freed, as Docker's
	}{
		{"only shared and recent records", `{"ID":"s","LastUsedAt":"12 days ago","Reclaimable":true,"Shared":true,"Size":"30GB","Type":"regular"}
{"ID":"r","LastUsedAt":"About an hour ago","Reclaimable":true,"Shared":false,"Size":"20GB","Type":"regular"}`, "", false, 0, ""},
		{"Docker removes nothing", duRecords, "Total:\t0B\n", true, 0, ""},
		{"Docker removes empty records", duRecords, removed, true, 1, "Pruned Docker (0 MiB by Docker's count)"},
		{"Docker prints no total", duRecords, "", true, 1, "Pruned Docker (Docker printed no total)"},
		// a size that cannot be read is no reason to stop Docker's own prune
		{"du fails", "ERR", "Total:\t1GB\n", true, 1, "Pruned Docker (954 MiB by Docker's count)"},
		{"du prints nothing this build reads", "ID  RECLAIMABLE  SIZE\nx  true  1GB\n", "Total:\t1GB\n", true, 1,
			"Pruned Docker (954 MiB by Docker's count)"},
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
				if x.Title != c.title {
					t.Errorf("%s: title %q, want %q", c.name, x.Title, c.title)
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
	asideAtPrune      *bool // whether an old lock file was set aside when uv's prune ran: the prune deletes it
}

// uvEnv is a uv process's environment; started is the one uv gives a command it starts: an environment's bin put
// first on uv's PATH.
var uvEnv = []string{"PATH=/usr/bin", "HOME=/home/u"}

func started(bin string, more ...string) []string {
	return append([]string{"PATH=" + bin + ":/usr/bin", "HOME=/home/u"}, more...)
}

// ran is the environment uv run gives its command: started's, with UV_RUN_RECURSION_DEPTH counted up.
func ran(bin string, more ...string) []string {
	return started(bin, append([]string{"UV_RUN_RECURSION_DEPTH=1"}, more...)...)
}

// newUVLock makes the cache, then the processes procs gives for it, of which holders hold its lock.
func newUVLock(t *testing.T, procs func(cache, lock string) []fakeProc, holders ...int) *uvLockFixture {
	t.Helper()
	return newUVLockIn(t, filepath.Join(t.TempDir(), "uv"), procs, holders...)
}

// newUVLockIn is newUVLock with the cache at a path of the test's choosing.
func newUVLockIn(t *testing.T, cache string, procs func(cache, lock string) []fakeProc, holders ...int) *uvLockFixture {
	t.Helper()
	f := &uvLockFixture{cache: cache, lockedAtPrune: new(bool), handedLock: new(bool), asideAtPrune: new(bool)}
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

// lets drops a holder's lock from the locks file, as its end does.
func (f *uvLockFixture) lets(t *testing.T, pid int) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.proc, "locks"))
	must(t, err)
	var keep []string
	for _, l := range strings.Split(string(b), "\n") {
		if fs := strings.Fields(l); len(fs) > 4 && fs[4] == strconv.Itoa(pid) {
			continue
		}
		keep = append(keep, l)
	}
	write(t, filepath.Join(f.proc, "locks"), strings.Join(keep, "\n"))
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
		*u.f.asideAtPrune = exists(u.f.lock + uvAsideSuffix)
		// as uv's prune does, it removes every file at the top of the cache but its markers and its lock
		ents, _ := os.ReadDir(u.f.cache)
		for _, e := range ents {
			if !e.IsDir() && !slices.Contains([]string{"CACHEDIR.TAG", ".gitignore", ".git", ".lock"}, e.Name()) {
				os.Remove(filepath.Join(u.f.cache, e.Name()))
			}
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
			env: ran("/proj/.venv/bin", "VIRTUAL_ENV=/proj/.venv")}}
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
	if !g.Done || !f.pruned(true) || f.pruned(false) || strings.Contains(g.Detail, "old lock file") || *f.asideAtPrune {
		t.Fatalf("with every holder outside the cache, the prune runs with --force: %+v %v", g, f.ran)
	}
	if !*f.lockedAtPrune {
		t.Error("the prune runs under a lock in the lock file's place, which a new uv process waits for")
	}
	var st syscall.Stat_t
	must(t, syscall.Stat(f.lock, &st))
	if st.Ino == f.id.ino || st.Mode&0o777 != 0o666 || exists(f.lock+uvAsideSuffix) {
		t.Errorf("the lock file in place is a new file with the old one's mode, and none is set aside (uv's prune would "+
			"delete it; the holders run outside the cache): inode %d (was %d), mode %o", st.Ino, f.id.ino, st.Mode&0o777)
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
	// what runs from the cache at the look gets a prune around it (uvaround_test.go); what comes after the look keeps
	// the prune
	for _, c := range []struct {
		name    string
		procs   func(cache, lock string) []fakeProc
		holders []int
		after   func(*testing.T, *uvLockFixture)
		why     string
	}{
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
			if f.pruned(true) || f.pruned(false) || g.Done || g.Action != ActKeep || !strings.Contains(g.Why, "waits for an install") ||
				strings.Contains(g.Why, "whose command") {
				t.Fatalf("a uv process that may write the cache now is waited for quietly, by the look: %s %q %v", g.Action, g.Why, f.ran)
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
			if f.pruned(true) || f.pruned(false) || g.Done || g.Action != ActKeep || !strings.Contains(g.Why, argvHead(c.uv)+" (pid 8)") ||
				strings.Contains(g.Why, "whose command") {
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
			fh, err := os.Open(f.lock)
			must(t, err)
			defer fh.Close()
			if err := syscall.Flock(int(fh.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
				t.Errorf("a uv process that starts gets the lock: %v", err)
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
			fakeProc{pid: 9, ppid: 8, argv: []string{"srv"}, env: ran("/opt/srv/bin")},
			fakeProc{pid: 10, ppid: 9, argv: []string{"sudo", "helper"}, env: ran("/opt/srv/bin")})
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
	for _, c := range []struct {
		name string
		env  func(t *testing.T, base, cache string) []string
		kept string // the entry it links to, which the prune around it keeps; "" waits
	}{
		{"packages linked into the cache", func(t *testing.T, base, cache string) []string {
			site := filepath.Join(base, "proj", ".venv", "lib", "python3.12", "site-packages")
			target := filepath.Join(cache, "archive-v0", "w", "pkg")
			must(t, os.MkdirAll(target, 0o755))
			must(t, os.MkdirAll(site, 0o755))
			must(t, os.Symlink(target, filepath.Join(site, "pkg")))
			return ran(filepath.Join(base, "proj", ".venv", "bin"), "VIRTUAL_ENV="+filepath.Join(base, "proj", ".venv"))
		}, "archive-v0/w"},
		{"packages as uv's symlink link mode lays them out", func(t *testing.T, base, cache string) []string {
			// uv makes a package's directories and links its files
			site := filepath.Join(base, "proj", ".venv", "lib", "python3.12", "site-packages")
			arch := filepath.Join(cache, "archive-v0", "w")
			write(t, filepath.Join(arch, "pkg", "__init__.py"), "")
			must(t, os.MkdirAll(filepath.Join(site, "pkg"), 0o755))
			must(t, os.Symlink(filepath.Join(arch, "pkg", "__init__.py"), filepath.Join(site, "pkg", "__init__.py")))
			return ran(filepath.Join(base, "proj", ".venv", "bin"), "VIRTUAL_ENV="+filepath.Join(base, "proj", ".venv"))
		}, "archive-v0/w"},
		{"an environment that is a link into the cache", func(t *testing.T, base, cache string) []string {
			target := filepath.Join(cache, "environments-v2", "proj-1")
			must(t, os.MkdirAll(filepath.Join(target, "bin"), 0o755))
			write(t, filepath.Join(target, "pyvenv.cfg"), "home = /opt/python/bin\n")
			must(t, os.MkdirAll(filepath.Join(base, "proj2"), 0o755))
			must(t, os.Symlink(target, filepath.Join(base, "proj2", ".venv")))
			return ran(filepath.Join(base, "proj2", ".venv", "bin"))
		}, "environments-v2/proj-1"},
		{"uv's symlink link mode", func(t *testing.T, base, cache string) []string {
			return ran("/proj/.venv/bin", "UV_LINK_MODE=symlink")
		}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			base := t.TempDir()
			procs := func(cache, lock string) []fakeProc {
				return append(outside(cache, lock), fakeProc{pid: 8, ppid: 1, argv: []string{"uv", "run", "srv"}, fds: []string{lock}, env: uvEnv},
					fakeProc{pid: 9, ppid: 8, argv: []string{"python", "srv"}, env: c.env(t, base, cache)})
			}
			f := newUVLock(t, procs, 4, 6, 8)
			uvEntries(t, f.cache, "archive-v0/stale")
			g := f.run(t, options(t, f.proc, time.Now(), nil))
			if c.kept == "" {
				if f.pruned(true) || g.Done || g.Action != ActReport || !strings.Contains(g.Why, "cannot tell what these use") ||
					!strings.Contains(g.Why, "uv run srv (pid 8)") {
					t.Fatalf("a process whose packages may be links into the cache wherever they are is one the look cannot tell: %s %q %v",
						g.Action, g.Why, f.ran)
				}
				left(t, f.cache, []string{"archive-v0/stale"}, nil)
				return
			}
			if !f.around(g) {
				t.Fatalf("a process whose files are links into the cache uses what they link to: %s %q %v", g.Action, g.Why, f.ran)
			}
			left(t, f.cache, []string{c.kept}, []string{"archive-v0/stale"})
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
	uvEntries(t, f.cache, "archive-v0/x", "archive-v0/stale")
	link := filepath.Join(t.TempDir(), "uv-link") // uv names the cache through a link; /proc shows where it is
	must(t, os.Symlink(real, link))
	o := options(t, f.proc, time.Now(), nil)
	o.Tools = f.tools(map[string]string{"uv cache dir": link + "\n"})
	g := f.runWith(t, o, link)
	if !f.around(g) {
		t.Fatalf("a mapped file of the cache, seen by its real path, is a use: %s %q %v", g.Action, g.Why, f.ran)
	}
	left(t, f.cache, []string{"archive-v0/x"}, []string{"archive-v0/stale"})
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
	// it counts as in use: only a read would say where it is
	if g := f.run(t, o); g.Done || f.pruned(true) || len(*resolved) > 0 || !strings.Contains(g.Why, "/opt/env2 came after the lock went in") {
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
		openers(f.proc, f.id, c.isLock, renamed, &Sample{Table: map[int]Proc{}}, statLock)
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
	held, _, why := uvSwap{lk: uvLockState{file: f.lock, id: lockID{f.id.major, f.id.minor, f.id.ino + 7}}, procRoot: f.proc}.lockInPlace(nil, false)
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

func TestUVsReportOnStderrIsWhatItsPruneFreedAndItsAnswersStayItsOwn(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	tool := filepath.Join(dir, "uv")
	// as uv does, the prune's report goes to stderr, and an answer to stdout after any warning on stderr
	write(t, tool, "#!/bin/sh\nif [ \"$2\" = prune ]; then echo 'Pruning cache at: /c/uv' >&2; "+
		"printf 'Removed 412 files (48.0KiB)\\n' >&2; else echo 'warning: a setting is ignored' >&2; echo /c/uv; fi\n")
	must(t, os.Chmod(tool, 0o755))
	ctx := context.Background()
	out, err := execTools{uv: tool}.UV(ctx, 5*time.Second, "cache", "prune", "--ci")
	if n, said, _ := uvReclaimed(out); err != nil || !said || n != 48<<10 {
		t.Errorf("uv's report on stderr is what its prune freed: %d %v %v %q", n, said, err, out)
	}
	if out, err := (execTools{uv: tool}).UV(ctx, 5*time.Second, "cache", "dir"); err != nil || lastLine(out) != "/c/uv" {
		t.Errorf("an answer on stdout is still the last line: %q %v", out, err)
	}
}

func TestAUVProcessPastItsSyncIsKnownHoweverItIsStarted(t *testing.T) {
	cmd, runCmd := started("/opt/tools/srv/bin"), ran("/opt/tools/srv/bin")
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
		{"a global option", []string{"uv", "--directory", "/srv/mcp", "run", "server.py"}, uvEnv, runCmd},
		{"global flags only", []string{"uv", "-q", "--offline", "run", "server.py"}, uvEnv, runCmd},
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
	uvEntries(t, f.cache, "archive-v0/x", "archive-v0/stale")
	o := options(t, f.proc, time.Now(), nil)
	write(t, o.MountInfo, "29 1 259:3 / / rw - ext4 /dev/sda2 rw\n40 29 252:0 / "+filepath.Join(base, "data")+
		" rw - ext4 /dev/mapper/data rw\n41 29 252:0 /cache "+filepath.Join(base, "home", ".cache")+" rw - ext4 /dev/mapper/data rw\n")
	if g := f.run(t, o); !f.around(g) {
		t.Fatalf("a PATH that names the cache through its bind mount runs from it: %s %q %v", g.Action, g.Why, f.ran)
	}
	left(t, f.cache, []string{"archive-v0/x"}, []string{"archive-v0/stale"})
}

func TestACommandThatSetItsTitleHasStarted(t *testing.T) {
	// setproctitle (gunicorn, Celery, vLLM) writes the title over the memory /proc reads an environment from: the
	// command's reads as NULs from then on, while uv's own reads as before
	zeroed := []string{strings.Repeat("\x00", 512)}
	for name, fromCache := range map[string]bool{"outside the cache": false, "from the cache": true} {
		t.Run(name, func(t *testing.T) {
			procs := func(cache, lock string) []fakeProc {
				var maps []string
				if fromCache { // the extension is mapped from the environment it runs in
					maps = []string{filepath.Join(cache, "archive-v0", "x", "lib", "python3.12", "site-packages", "_setproctitle.so")}
				}
				return append(outside(cache, lock),
					fakeProc{pid: 8, ppid: 1, argv: []string{"uv", "run", "srv"}, fds: []string{lock}, env: uvEnv},
					fakeProc{pid: 9, ppid: 8, argv: []string{"srv: master"}, exe: "/opt/python/bin/python3", maps: maps, env: zeroed})
			}
			f := newUVLock(t, procs, 4, 6, 8)
			uvEntries(t, f.cache, "archive-v0/x", "archive-v0/stale")
			g := f.run(t, options(t, f.proc, time.Now(), nil))
			switch {
			case !fromCache && (!g.Done || !f.pruned(true)):
				t.Fatalf("a run whose command set its title has started it, here outside the cache: %s %q %v", g.Action, g.Why, f.ran)
			case fromCache && !f.around(g):
				t.Fatalf("one that runs from the cache gets a prune around what it maps: %s %q %v", g.Action, g.Why, f.ran)
			case fromCache:
				left(t, f.cache, []string{"archive-v0/x"}, []string{"archive-v0/stale"})
			}
		})
	}
}

func TestAQuietWaitThatLastsADayIsReported(t *testing.T) {
	for _, c := range []struct {
		name  string
		start int
		want  string
	}{
		{"an hour", 172800*userHZ - 3600*userHZ, ActKeep},
		{"two days", 100, ActReport},
	} {
		t.Run(c.name, func(t *testing.T) {
			procs := func(cache, lock string) []fakeProc { // a tool server whose command set its own PATH looks unstarted
				return append(outside(cache, lock), fakeProc{pid: 8, ppid: 1, argv: []string{"uv", "tool", "uvx", "srv"}, fds: []string{lock},
					env: uvEnv, start: c.start}, fakeProc{pid: 9, ppid: 8, argv: []string{"srv"}, env: []string{"PATH=/opt/srv/wrap:/usr/bin"}})
			}
			f := newUVLock(t, procs, 4, 6, 8)
			write(t, filepath.Join(f.proc, "uptime"), "172800.00 1000.00\n") // two days after boot
			g := f.run(t, options(t, f.proc, time.Now(), nil))
			if f.pruned(true) || g.Done || g.Action != c.want || !strings.Contains(g.Why, "uv tool uvx srv (pid 8)") {
				t.Fatalf("a quiet wait of %s: %s %q %v", c.name, g.Action, g.Why, f.ran)
			}
		})
	}
}

func TestAHolderInAnotherMountNamespaceIsJudgedByItsOwnView(t *testing.T) {
	for _, c := range []struct {
		name string
		root string // where the holder's namespace is rooted
		done bool
	}{
		{"a namespace that binds the cache elsewhere", "", false},
		{"a sandbox that sees the cache where the run does", "/", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			procs := func(cache, lock string) []fakeProc { // it names the cache by its own paths
				return append(outside(cache, lock),
					fakeProc{pid: 8, ppid: 1, argv: []string{"uv", "tool", "uvx", "srv"}, fds: []string{"/cache/uv/.lock"}, env: uvEnv},
					fakeProc{pid: 9, ppid: 8, argv: []string{"/cache/uv/archive-v0/x/bin/python", "srv"}, exe: "/usr/bin/python3.12",
						maps: []string{"/cache/uv/archive-v0/x/lib/_ext.so"}, env: started("/cache/uv/archive-v0/x/bin")})
			}
			f := newUVLock(t, procs, 4, 6, 8)
			ns := func(pid, link string) {
				must(t, os.MkdirAll(filepath.Join(f.proc, pid, "ns"), 0o755))
				must(t, os.Symlink(link, filepath.Join(f.proc, pid, "ns", "mnt")))
			}
			for _, pid := range []string{"8", "9"} {
				ns(pid, "mnt:[4026533333]")
				root := c.root
				if root == "" {
					root = t.TempDir()
				}
				must(t, os.Symlink(root, filepath.Join(f.proc, pid, "root")))
			}
			for _, pid := range []string{"4", "5", "6", "7", strconv.Itoa(os.Getpid())} {
				ns(pid, "mnt:[4026531841]")
			}
			g := f.run(t, options(t, f.proc, time.Now(), nil))
			if g.Done != c.done || g.Done != f.pruned(true) || !c.done && !strings.Contains(g.Why, "another mount namespace") {
				t.Fatalf("a holder in another mount namespace is judged by the paths it sees: %s %q %v", g.Action, g.Why, f.ran)
			}
		})
	}
}

func TestABuildBackendIsNotTheCommandItsUVStarts(t *testing.T) {
	for _, c := range []struct {
		name  string
		uv    []string
		uvEnv []string
	}{
		{"a uv run", []string{"uv", "run", "srv"}, uvEnv},
		{"a uvx", []string{"uv", "tool", "uvx", "srv"}, uvEnv},
	} {
		t.Run(c.name, func(t *testing.T) {
			// a local package built without isolation: the backend gets the target environment's bin first on uv's PATH,
			// as a command would, and only its -c script names the cache
			procs := func(cache, lock string) []fakeProc {
				return append(outside(cache, lock), fakeProc{pid: 8, ppid: 1, argv: c.uv, fds: []string{lock}, env: c.uvEnv},
					fakeProc{pid: 9, ppid: 8, argv: []string{"/proj/.venv/bin/python", "-c", "import backend"}, cwd: "/proj",
						env: started("/proj/.venv/bin", "VIRTUAL_ENV=/proj/.venv", "UV_INTERNAL__BUILD_DIR=/proj")})
			}
			f := newUVLock(t, procs, 4, 6, 8)
			g := f.run(t, options(t, f.proc, time.Now(), nil))
			if f.pruned(true) || g.Done || g.Action != ActKeep || !strings.Contains(g.Why, argvHead(c.uv)+" (pid 8)") {
				t.Fatalf("a uv process whose child is a build backend is at its sync: %s %q %v", g.Action, g.Why, f.ran)
			}
		})
	}
}

func TestAPlaceThatDoesNotAnswerKeepsThePrune(t *testing.T) {
	defer func(d time.Duration) { uvPlaceWait = d }(uvPlaceWait)
	uvPlaceWait = 50 * time.Millisecond
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	was := resolveLinks
	t.Cleanup(func() { resolveLinks = was })
	resolveLinks = func(p string) (string, error) {
		if p == "/mnt/dead/env" { // a dead network mount: the call never returns
			<-stuck
		}
		return was(p)
	}
	procs := func(cache, lock string) []fakeProc {
		return append(outside(cache, lock), fakeProc{pid: 18, ppid: 1, argv: []string{"python"}, env: []string{"VIRTUAL_ENV=/mnt/dead/env"}})
	}
	f := newUVLock(t, procs, 4, 6)
	start := time.Now()
	g := f.run(t, options(t, f.proc, time.Now(), nil))
	if f.pruned(true) || g.Done || !strings.Contains(g.Why, "/mnt/dead/env did not answer") || time.Since(start) > 10*time.Second {
		t.Fatalf("a place that does not answer may be in use, and the run goes on: %s %q after %s", g.Action, g.Why, time.Since(start))
	}
}

func TestAHolderOrUVProcessThatCameBeforeTheRenameKeepsTheLockFileInPlace(t *testing.T) {
	for _, c := range []struct {
		name, why string
		root      bool
		came      func(*testing.T, *uvLockFixture)
	}{
		{"a holder", "took the cache's lock since the look", true, func(t *testing.T, f *uvLockFixture) {
			f.addProc(t, 77, 1, "uv\x00run\x00x\x00", "")
			f.holds(t, 77)
		}},
		{"a uv process the run may not read", "started since the look may have the cache's lock open", false, func(t *testing.T, f *uvLockFixture) {
			must(t, os.Chmod(filepath.Join(f.addProc(t, 30, 29, "uv\x00sync\x00", ""), "fd"), 0))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if !c.root && os.Geteuid() == 0 {
				t.Skip("root reads every process")
			}
			f := newUVLock(t, outside, 4, 6)
			t.Cleanup(func() { os.Chmod(filepath.Join(f.proc, "30", "fd"), 0o755) })
			armed := false
			o := options(t, f.proc, time.Now(), nil)
			o.Tools = dockerHook{f.tools(nil), &armed, func() { c.came(t, f) }} // during the look, before the rename
			g := f.runWith(t, o, f.cache)
			if g.Done || f.pruned(true) || !strings.Contains(g.Why, c.why) {
				t.Fatalf("%s that came before the rename keeps the lock file in place: %s %q", c.name, g.Action, g.Why)
			}
			if id, err := fileID(f.lock); err != nil || id != f.id {
				t.Errorf("the lock file stays the one every uv process opens: %v %v", id, err)
			}
		})
	}
}

func TestAContainerThatMountsAPartOfTheCacheIsWaitedFor(t *testing.T) {
	f := newUVLock(t, outside, 4, 6)
	o := options(t, f.proc, time.Now(), nil)
	o.Tools = f.tools(map[string]string{"docker ps": "c1\n", "docker inspect": "/box\t" + filepath.Join(f.cache, "archive-v0", "x") + "\n",
		"docker info": "ERR"})
	if g := f.runWith(t, o, f.cache); f.pruned(true) || f.pruned(false) || !strings.Contains(g.Why, "container box mounts") {
		t.Fatalf("a container that mounts a part of the cache may run from it: %s %q %v", g.Action, g.Why, f.ran)
	}
}

func TestAProcessIDAnotherProcessTookIsNotTheOneTheLookSaw(t *testing.T) {
	for _, c := range []struct {
		name  string
		after func(*testing.T, *uvLockFixture)
		done  bool
	}{
		// the installed tool ended and its pid went to another process: the old holder holds nothing now
		{"a holder's", func(t *testing.T, f *uvLockFixture) {
			must(t, os.RemoveAll(filepath.Join(f.proc, "4")))
			must(t, os.RemoveAll(filepath.Join(f.proc, "5")))
			f.lets(t, 4)
			f.addProc(t, 4, 1, "bash\x00", "PATH=/usr/bin\x00")
		}, true},
		// the tool's command ended and another, with the same pid, runs: it may be at its sync
		{"a command's", func(t *testing.T, f *uvLockFixture) {
			must(t, os.RemoveAll(filepath.Join(f.proc, "5")))
			f.addProc(t, 5, 4, "python\x00pkg\x00", "PATH=/opt/tools/pkg/bin:/usr/bin\x00")
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newUVLock(t, outside, 4, 6)
			o := options(t, f.proc, time.Now(), nil)
			o.afterUVLock = func() { c.after(t, f) }
			if g := f.run(t, o); g.Done != c.done || g.Done != f.pruned(true) {
				t.Fatalf("a process is the one the look saw by its pid and its start: %s %q %v", g.Action, g.Why, f.ran)
			}
		})
	}
}

func TestAUVProcessThatTookTheIDOfOneTheLookSawIsNew(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every process")
	}
	procs := func(cache, lock string) []fakeProc { // a container's uv the run may not read
		return append(outside(cache, lock), fakeProc{pid: 30, ppid: 29, argv: []string{"uv", "run", "app"}, env: uvEnv},
			fakeProc{pid: 31, ppid: 30, argv: []string{"python", "app"}, env: ran("/app/.venv/bin")})
	}
	f := newUVLock(t, procs, 4, 6)
	for _, p := range []string{"30/fd", "30/environ", "31/fd", "31/environ"} {
		must(t, os.Chmod(filepath.Join(f.proc, p), 0))
	}
	o := options(t, f.proc, time.Now(), nil)
	o.afterUVLock = func() { // it ended, and a uv process the run may not read took its pid
		must(t, os.Chmod(filepath.Join(f.proc, "30", "fd"), 0o755))
		must(t, os.RemoveAll(filepath.Join(f.proc, "30")))
		must(t, os.Chmod(filepath.Join(f.addProc(t, 30, 29, "uv\x00cache\x00clean\x00", ""), "fd"), 0))
	}
	t.Cleanup(func() {
		for _, p := range []string{"30/fd", "31/fd"} {
			os.Chmod(filepath.Join(f.proc, p), 0o755)
		}
	})
	if g := f.run(t, o); g.Done || f.pruned(true) || !strings.Contains(g.Why, "uv process (pid 30) that started as capacity put its own in place") {
		t.Fatalf("a uv process with the pid of one the look saw, and another start, is new: %s %q %v", g.Action, g.Why, f.ran)
	}
}

func TestAPrunesNotesKeepWhyItMayNotPrune(t *testing.T) {
	f := newUVLock(t, outside, 4, 6)
	o := options(t, f.proc, time.Now(), fakeJobs{err: fmt.Errorf("a unit file cannot be read")})
	o.Tools, o.Rules = f.tools(nil), []string{RuleUV}
	res, err := Run(context.Background(), Config{Live: []string{RuleUV}}.Normal(), openStore(t), o)
	must(t, err)
	for _, rr := range res.Rules {
		if rr.Rule == RuleUV && (!slices.Contains(rr.Notes, "no prune: the scheduled jobs could not all be read") || f.pruned(true) || f.pruned(false)) {
			t.Errorf("the rule's notes keep why it may not prune: %q %v", rr.Notes, f.ran)
		}
	}
}

func TestATitledCommandOfARunThatLayersKeepsEveryOverlay(t *testing.T) {
	// uv run --with runs its command from a layer in the cache over the project's .venv: setproctitle maps from the
	// .venv, the layer's pure-Python modules were read and closed, and the environment that named the layer is gone.
	// The overlay in builds-v0 the command runs in names its layer, and the prune around it keeps both.
	zeroed := []string{strings.Repeat("\x00", 512)}
	for _, c := range []struct {
		name string
		argv []string
		done bool
	}{
		{"--with", []string{"uv", "run", "--with", "six", "python", "srv.py"}, false},
		{"-w", []string{"uv", "run", "-wsix", "python", "srv.py"}, false},
		{"-w among other short flags", []string{"uv", "run", "-qw", "six", "python", "srv.py"}, false},
		{"--with-requirements", []string{"uv", "run", "--with-requirements=r.txt", "python", "srv.py"}, false},
		{"a run that layers nothing", []string{"uv", "run", "--", "srv", "-w", "4"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			procs := func(cache, lock string) []fakeProc {
				return append(outside(cache, lock),
					fakeProc{pid: 8, ppid: 1, argv: c.argv, fds: []string{lock}, cwd: "/proj", env: uvEnv},
					fakeProc{pid: 9, ppid: 8, argv: []string{"srv: master"}, exe: "/usr/bin/python3.12", cwd: "/proj",
						maps: []string{"/proj/.venv/lib/python3.12/site-packages/setproctitle/_setproctitle.so"}, env: zeroed})
			}
			f := newUVLock(t, procs, 4, 6, 8)
			uvEntries(t, f.cache, "archive-v0/envlayer", "archive-v0/stale")
			if !c.done { // the overlay the layered run's command runs in
				uvEntries(t, f.cache, "builds-v0/.tmpOv1")
				write(t, filepath.Join(f.cache, "builds-v0", ".tmpOv1", "lib", "python3.12", "site-packages", "_uv_ephemeral_overlay.pth"),
					`import site; site.addsitedir(__import__("os").fsdecode(b"`+filepath.Join(f.cache, "archive-v0", "envlayer", "lib")+`"))`+"\n")
			}
			g := f.run(t, options(t, f.proc, time.Now(), nil))
			switch {
			case c.done && (!g.Done || !f.pruned(true)):
				t.Fatalf("a run that layers nothing is judged by its command's files: %s %q %v", g.Action, g.Why, f.ran)
			case !c.done && !f.around(g):
				t.Fatalf("a layered run's command whose environment is gone gets a prune around every overlay: %s %q %v", g.Action, g.Why, f.ran)
			case !c.done:
				left(t, f.cache, []string{"builds-v0/.tmpOv1", "archive-v0/envlayer"}, []string{"archive-v0/stale"})
			}
		})
	}
}

func TestAHolderThatCameDuringTheScanKeepsTheLockFileInPlace(t *testing.T) {
	f := newUVLock(t, outside, 4, 6)
	defer func(s func(string) (os.FileInfo, error)) { statLock = s }(statLock)
	came := false
	statLock = func(p string) (os.FileInfo, error) {
		if !came && strings.HasPrefix(p, f.proc) { // the scan stats its first link that names the lock file
			came = true
			f.addProc(t, 77, 1, "uv\x00run\x00x\x00", "")
			f.holds(t, 77)
		}
		return os.Stat(p)
	}
	g := f.run(t, options(t, f.proc, time.Now(), nil))
	if g.Done || f.pruned(true) || !strings.Contains(g.Why, "took the cache's lock since the look") {
		t.Fatalf("a uv process that took the lock during the scan before the rename keeps it: %s %q", g.Action, g.Why)
	}
	if id, err := fileID(f.lock); err != nil || id != f.id {
		t.Errorf("the lock file it holds stays in place: %v %v", id, err)
	}
}

func TestAHolderWhosePIDAnotherProcessTookIsNew(t *testing.T) {
	f := newUVLock(t, outside, 4, 6)
	armed := false
	o := options(t, f.proc, time.Now(), nil)
	o.Tools = dockerHook{f.tools(nil), &armed, func() { // the tool ended; a uv process took its pid, and the lock
		must(t, os.RemoveAll(filepath.Join(f.proc, "4")))
		must(t, os.RemoveAll(filepath.Join(f.proc, "5")))
		f.addProc(t, 4, 1, "uv\x00run\x00x\x00", "")
	}}
	g := f.runWith(t, o, f.cache)
	if g.Done || f.pruned(true) || !strings.Contains(g.Why, "uv process (pid 4) took the cache's lock since the look") {
		t.Fatalf("a holder is the one the look saw by its pid and its start: %s %q", g.Action, g.Why)
	}
	if id, err := fileID(f.lock); err != nil || id != f.id {
		t.Errorf("the lock file stays in place: %v %v", id, err)
	}
}

// withChildren writes each fixture process's task/PID/children, as the kernel lists them, from the processes' stat.
func withChildren(t *testing.T, f *uvLockFixture) {
	t.Helper()
	kids := map[string][]string{}
	ents, err := os.ReadDir(f.proc)
	must(t, err)
	for _, e := range ents {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		if st, err := os.ReadFile(filepath.Join(f.proc, e.Name(), "stat")); err == nil {
			pp := strconv.Itoa(parentOf(string(st)))
			kids[pp] = append(kids[pp], e.Name())
		}
	}
	for _, e := range ents {
		if _, err := strconv.Atoi(e.Name()); err == nil {
			write(t, filepath.Join(f.proc, e.Name(), "task", e.Name(), "children"), strings.Join(kids[e.Name()], " ")+" ")
		}
	}
}

// inNamespaces gives the fixture's processes mount namespaces: those named in other the namespace of another, the
// rest the run's.
func inNamespaces(t *testing.T, f *uvLockFixture, other ...string) {
	t.Helper()
	ents, err := os.ReadDir(f.proc)
	must(t, err)
	for _, e := range ents {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		ns := "mnt:[4026531841]"
		if slices.Contains(other, e.Name()) {
			ns = "mnt:[4026533333]"
		}
		must(t, os.MkdirAll(filepath.Join(f.proc, e.Name(), "ns"), 0o755))
		must(t, os.Symlink(ns, filepath.Join(f.proc, e.Name(), "ns", "mnt")))
	}
}

func TestTheLookUnderTheLockReadsNoNewNamespace(t *testing.T) {
	f := newUVLock(t, outside, 4, 6)
	inNamespaces(t, f)
	o := options(t, f.proc, time.Now(), nil)
	o.afterUVLock = func() { // the tool's command starts a sandbox that sees the cache where the run does
		dir := f.addProc(t, 16, 5, "bwrap\x00srv\x00", "PATH=/usr/bin\x00")
		must(t, os.MkdirAll(filepath.Join(dir, "ns"), 0o755))
		must(t, os.Symlink("mnt:[4026533333]", filepath.Join(dir, "ns", "mnt")))
		must(t, os.Symlink("/", filepath.Join(dir, "root")))
	}
	if g := f.run(t, o); g.Done || f.pruned(true) || !strings.Contains(g.Why, "another mount namespace") {
		t.Fatalf("a namespace the first look did not answer is not read under the lock: %s %q %v", g.Action, g.Why, f.ran)
	}
}

func TestANamespaceThatSeesTheCacheAsUVNamesItIsJudgedByItsView(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	link := filepath.Join(dir, "uv-link") // uv names the cache through a link
	procs := func(cache, lock string) []fakeProc {
		return append(outside(cache, lock), fakeProc{pid: 8, ppid: 1, argv: []string{"uv", "tool", "uvx", "srv"},
			fds: []string{filepath.Join(link, ".lock")}, env: uvEnv}, fakeProc{pid: 9, ppid: 8, argv: []string{"srv"}, env: started("/opt/srv/bin")})
	}
	f := newUVLockIn(t, filepath.Join(dir, "uv"), procs, 4, 6, 8)
	must(t, os.Symlink(f.cache, link))
	inNamespaces(t, f, "8", "9")
	root := t.TempDir() // the sandbox has the lock file only where uv names it
	must(t, os.MkdirAll(filepath.Join(root, link), 0o755))
	must(t, os.Link(f.lock, filepath.Join(root, link, ".lock")))
	for _, pid := range []string{"8", "9"} {
		must(t, os.Symlink(root, filepath.Join(f.proc, pid, "root")))
	}
	o := options(t, f.proc, time.Now(), nil)
	o.Tools = f.tools(map[string]string{"uv cache dir": link + "\n"})
	if g := f.runWith(t, o, link); !g.Done || !f.pruned(true) {
		t.Fatalf("a sandbox that sees the lock file where uv names it is judged by its paths: %s %q %v", g.Action, g.Why, f.ran)
	}
}

func TestALookUnderTheLockThatDoesNotEndLetsTheLockGo(t *testing.T) {
	defer func(d time.Duration) { uvUnderLockWait = d }(uvUnderLockWait)
	uvUnderLockWait = 200 * time.Millisecond
	procs := func(cache, lock string) []fakeProc { // it has the old lock file open, so the look after the rename stats it
		return append(outside(cache, lock), fakeProc{pid: 13, ppid: 1, argv: []string{"server"}, fds: []string{lock + " (deleted)"}})
	}
	f := newUVLock(t, procs, 4, 6)
	write(t, f.lock+" (deleted)", "")
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	defer func(s func(string) (os.FileInfo, error)) { statLock = s }(statLock)
	statLock = func(p string) (os.FileInfo, error) {
		var st syscall.Stat_t
		if strings.HasSuffix(p, filepath.Join("13", "fd", "3")) && syscall.Stat(f.lock, &st) == nil && st.Ino != f.id.ino {
			<-stuck // after the rename, a read of /proc that does not answer
		}
		return os.Stat(p)
	}
	start := time.Now()
	g := f.run(t, options(t, f.proc, time.Now(), nil))
	if g.Done || f.pruned(true) || !strings.Contains(g.Why, "did not end in 200ms") || time.Since(start) > 5*time.Second {
		t.Fatalf("a look under the lock that does not end lets it go: %s %q after %s", g.Action, g.Why, time.Since(start))
	}
	fh, err := os.Open(f.lock)
	must(t, err)
	defer fh.Close()
	if err := syscall.Flock(int(fh.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		t.Errorf("a uv process that starts gets the lock: %v", err)
	}
}

func TestATitleThatSpillsIntoTheEnvironmentOfALayeredRunHoldsNoPATH(t *testing.T) {
	// a title longer than the command line runs on over the environment: /proc reads its tail, an = in it
	spilled := []string{"wwwwwwww --prefetch-multiplier=4)" + strings.Repeat("\x00", 512)}
	procs := func(cache, lock string) []fakeProc {
		return append(outside(cache, lock),
			fakeProc{pid: 8, ppid: 1, argv: []string{"uv", "run", "--with", "six", "celery", "-A", "proj", "worker"},
				fds: []string{lock}, cwd: "/proj", env: uvEnv},
			fakeProc{pid: 9, ppid: 8, argv: []string{"[celeryd: celery@host:MainProcess] -active- (-A proj worker)"},
				exe: "/usr/bin/python3.12", cwd: "/proj",
				maps: []string{"/proj/.venv/lib/python3.12/site-packages/setproctitle/_setproctitle.so"}, env: spilled})
	}
	f := newUVLock(t, procs, 4, 6, 8)
	uvEntries(t, f.cache, "builds-v0/.tmpOv1", "archive-v0/stale")
	if g := f.run(t, options(t, f.proc, time.Now(), nil)); !f.around(g) {
		t.Fatalf("a layered run's command whose environment holds no PATH may import from the layer: %s %q %v", g.Action, g.Why, f.ran)
	}
	left(t, f.cache, []string{"builds-v0/.tmpOv1"}, []string{"archive-v0/stale"})
}

func TestAProcessThatCameAfterTheRenameFromALinkedEnvironmentKeepsThePrune(t *testing.T) {
	base := t.TempDir()
	f := newUVLock(t, outside, 4, 6)
	env := filepath.Join(f.cache, "environments-v2", "proj2-1") // a centralized project environment
	must(t, os.MkdirAll(filepath.Join(env, "bin"), 0o755))
	must(t, os.MkdirAll(filepath.Join(base, "proj2"), 0o755))
	venv := filepath.Join(base, "proj2", ".venv")
	must(t, os.Symlink(env, venv))
	o := options(t, f.proc, time.Now(), nil)
	o.afterUVLock = func() { // a script starts the other project's server directly
		f.addProc(t, 17, 1, venv+"/bin/python\x00w.py\x00", "VIRTUAL_ENV="+venv+"\x00PATH="+venv+"/bin:/usr/bin\x00")
	}
	if g := f.run(t, o); g.Done || f.pruned(true) {
		t.Fatalf("a process from an environment linked into the cache uses it, though only a read of the link says so: %s %q %v",
			g.Action, g.Why, f.ran)
	}
}

func TestAHolderThatProcHidesHasNotEnded(t *testing.T) {
	for _, c := range []struct {
		opt  string
		done bool
	}{
		{"hidepid=invisible", false}, {"hidepid=2", false}, {"hidepid=ptraceable", false},
		{"hidepid=noaccess", true}, // it shows every process's directory, so one it does not show ended
	} {
		t.Run(c.opt, func(t *testing.T) {
			f := newUVLock(t, outside, 4, 6, 4243) // /proc shows no 4243, and /proc/locks names it
			withChildren(t, f)
			o := options(t, f.proc, time.Now(), nil)
			write(t, o.MountInfo, "29 1 259:3 / / rw - ext4 /dev/sda2 rw\n30 29 0:25 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw,"+
				c.opt+"\n")
			if g := f.run(t, o); g.Done != c.done || g.Done != f.pruned(true) || !c.done && !strings.Contains(g.Why, "cannot read holds the lock (pid 4243)") {
				t.Fatalf("a holder a hiding /proc does not show is one the look cannot read: %s %q %v", g.Action, g.Why, f.ran)
			}
		})
	}
}

func TestWhatTheLookFindsBeforeTheRenameKeepsTheLockFileInPlace(t *testing.T) {
	for _, c := range []struct {
		name, why, action string
		came              func(*testing.T, *uvLockFixture)
	}{
		{"a holder's command ended", "whose command is not the one the look saw", ActKeep, func(t *testing.T, f *uvLockFixture) {
			must(t, os.RemoveAll(filepath.Join(f.proc, "5")))
		}},
		{"a process came to use the cache", "python3 (pid 12)", ActReport, func(t *testing.T, f *uvLockFixture) {
			dir := f.addProc(t, 12, 1, "python3\x00", "")
			write(t, filepath.Join(dir, "maps"), "7f-7f r--s 0 0:0 1 "+filepath.Join(f.cache, "archive-v0", "y", "lib.so")+"\n")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newUVLock(t, outside, 4, 6)
			armed := false
			o := options(t, f.proc, time.Now(), nil)
			o.Tools = dockerHook{f.tools(nil), &armed, func() { c.came(t, f) }} // during the look, before the swap
			g := f.runWith(t, o, f.cache)
			if g.Done || f.pruned(true) || g.Action != c.action || !strings.Contains(g.Why, c.why) {
				t.Fatalf("what the look finds before the rename keeps the prune from running: %s %q", g.Action, g.Why)
			}
			if id, err := fileID(f.lock); err != nil || id != f.id || exists(f.lock+".capacity") {
				t.Errorf("and the lock file in place, every holder on it: %v %v", id, err)
			}
		})
	}
}

func TestLocksThatCannotBeReadAgainKeepTheLockFile(t *testing.T) {
	f := newUVLock(t, outside, 4, 6)
	armed := false
	o := options(t, f.proc, time.Now(), nil)
	o.Tools = dockerHook{f.tools(nil), &armed, func() { must(t, os.Remove(filepath.Join(f.proc, "locks"))) }}
	g := f.runWith(t, o, f.cache)
	if g.Done || f.pruned(true) || !strings.Contains(g.Why, "cannot be read again") {
		t.Fatalf("no swap blind to the locks: %s %q %v", g.Action, g.Why, f.ran)
	}
	if id, err := fileID(f.lock); err != nil || id != f.id {
		t.Errorf("the lock file stays in place: %v %v", id, err)
	}
}

func TestAFileAnEarlierRunLeftBesideTheLockIsNoBar(t *testing.T) {
	f := newUVLock(t, outside, 4, 6)
	write(t, f.lock+".capacity", "") // a run that died before its rename
	if g := f.run(t, options(t, f.proc, time.Now(), nil)); !g.Done || !f.pruned(true) {
		t.Fatalf("a file an earlier run left is replaced: %s %q %v", g.Action, g.Why, f.ran)
	}
}

func TestAContainerThatMountsWhatReallyHoldsTheCacheIsWaitedFor(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	f := newUVLockIn(t, filepath.Join(dir, "data", "uv"), outside, 4, 6)
	link := filepath.Join(t.TempDir(), "uv-link") // uv names the cache through a link
	must(t, os.Symlink(f.cache, link))
	o := options(t, f.proc, time.Now(), nil)
	o.Tools = f.tools(map[string]string{"uv cache dir": link + "\n", "docker ps": "c1\n",
		"docker inspect": "/box\t" + filepath.Join(dir, "data") + "\n", "docker info": "ERR"})
	if g := f.runWith(t, o, link); f.pruned(true) || f.pruned(false) || !strings.Contains(g.Why, "container box mounts") {
		t.Fatalf("a container that mounts what really holds the cache may run from it: %s %q %v", g.Action, g.Why, f.ran)
	}
}

func TestANamespaceWhoseViewDoesNotAnswerIsAnotherView(t *testing.T) {
	defer func(d time.Duration) { uvPlaceWait = d }(uvPlaceWait)
	uvPlaceWait = 50 * time.Millisecond
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	defer func(s func(string) (os.FileInfo, error)) { statView = s }(statView)
	statView = func(p string) (os.FileInfo, error) { // a path walk in its view crosses a mount that does not answer
		<-stuck
		return nil, os.ErrNotExist
	}
	procs := func(cache, lock string) []fakeProc {
		return append(outside(cache, lock), fakeProc{pid: 8, ppid: 1, argv: []string{"uv", "tool", "uvx", "srv"}, fds: []string{lock}, env: uvEnv},
			fakeProc{pid: 9, ppid: 8, argv: []string{"srv"}, env: started("/opt/srv/bin")})
	}
	f := newUVLock(t, procs, 4, 6, 8)
	inNamespaces(t, f, "8", "9")
	start := time.Now()
	g := f.run(t, options(t, f.proc, time.Now(), nil))
	if g.Done || f.pruned(true) || !strings.Contains(g.Why, "another mount namespace") || time.Since(start) > 10*time.Second {
		t.Fatalf("a namespace whose view does not answer is not the run's: %s %q after %s", g.Action, g.Why, time.Since(start))
	}
}

func TestAUVRunInsideABuildBackendRunsItsCommand(t *testing.T) {
	procs := func(cache, lock string) []fakeProc { // a backend ran uv run: both have the backend's build dir
		inBuild := append([]string{"UV_INTERNAL__BUILD_DIR=/src/pkg"}, uvEnv...)
		return append(outside(cache, lock), fakeProc{pid: 8, ppid: 1, argv: []string{"uv", "run", "gen.py"}, fds: []string{lock}, env: inBuild},
			fakeProc{pid: 9, ppid: 8, argv: []string{"python", "gen.py"}, env: ran("/src/pkg/.venv/bin", "UV_INTERNAL__BUILD_DIR=/src/pkg")})
	}
	f := newUVLock(t, procs, 4, 6, 8)
	if g := f.run(t, options(t, f.proc, time.Now(), nil)); !g.Done || !f.pruned(true) {
		t.Fatalf("a uv run's command keeps the build dir its uv has, and is no backend: %s %q %v", g.Action, g.Why, f.ran)
	}
}

func TestAHiddenUVProcessThatTookTheLockBeforeTheRenameKeepsIt(t *testing.T) {
	f := newUVLock(t, outside, 4, 6)
	withChildren(t, f)
	armed := false
	o := options(t, f.proc, time.Now(), nil)
	write(t, o.MountInfo, "29 1 259:3 / / rw - ext4 /dev/sda2 rw\n30 29 0:25 / /proc rw - proc proc rw,hidepid=invisible\n")
	o.Tools = dockerHook{f.tools(nil), &armed, func() { f.holds(t, 4250) }} // another user's uv, which /proc hides
	g := f.runWith(t, o, f.cache)
	if g.Done || f.pruned(true) || !strings.Contains(g.Why, "uv process (pid 4250) took the cache's lock since the look") {
		t.Fatalf("a holder /proc hides that came before the rename is a new one: %s %q", g.Action, g.Why)
	}
	if id, err := fileID(f.lock); err != nil || id != f.id {
		t.Errorf("the lock file stays in place: %v %v", id, err)
	}
}

func TestTheRunsOwnLockFileIsNoUseOfTheCache(t *testing.T) {
	f := newUVLock(t, outside, 4, 6)
	// the run has capacity's new lock file open while it looks again before the rename, as /proc shows it
	must(t, os.Symlink(f.lock+".capacity", filepath.Join(f.proc, strconv.Itoa(os.Getpid()), "fd", "3")))
	if g := f.run(t, options(t, f.proc, time.Now(), nil)); !g.Done || !f.pruned(true) || !strings.Contains(g.Detail, "2 uv processes hold its lock") {
		t.Fatalf("the file capacity puts in the lock's place is its own: %s %q %q %v", g.Action, g.Why, g.Detail, f.ran)
	}
}

func TestTheDetailCountsTheHolders(t *testing.T) {
	one := func(cache, lock string) []fakeProc { return outside(cache, lock)[:2] } // the installed tool alone
	for want, c := range map[string]struct {
		procs   func(cache, lock string) []fakeProc
		holders []int
	}{"1 uv process holds its lock": {one, []int{4}}, "2 uv processes hold its lock": {outside, []int{4, 6}}} {
		f := newUVLock(t, c.procs, c.holders...)
		if g := f.run(t, options(t, f.proc, time.Now(), nil)); !g.Done || !strings.Contains(g.Detail, want) {
			t.Errorf("the detail says how many hold the lock (%s): %q %q", want, g.Detail, g.Why)
		}
	}
}

func TestACommandsOwnOptionsAreNoLayer(t *testing.T) {
	// every word after uv run's command is the command's: gunicorn's -w and Celery's --without-* are theirs, and both
	// set their title with setproctitle from the project's .venv
	zeroed := []string{strings.Repeat("\x00", 512)}
	for _, argv := range [][]string{
		{"uv", "run", "gunicorn", "-w", "4", "app:app"},
		{"uv", "run", "--python", "3.12", "celery", "-A", "proj", "worker", "--without-gossip"},
	} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			procs := func(cache, lock string) []fakeProc {
				return append(outside(cache, lock),
					fakeProc{pid: 8, ppid: 1, argv: argv, fds: []string{lock}, cwd: "/proj", env: uvEnv},
					fakeProc{pid: 9, ppid: 8, argv: []string{"gunicorn: master [app:app]"}, exe: "/usr/bin/python3.12", cwd: "/proj",
						maps: []string{"/proj/.venv/lib/python3.12/site-packages/setproctitle/_setproctitle.so"}, env: zeroed})
			}
			f := newUVLock(t, procs, 4, 6, 8)
			if g := f.run(t, options(t, f.proc, time.Now(), nil)); !g.Done || !f.pruned(true) {
				t.Fatalf("a run that layers nothing runs its titled command from the project's .venv: %s %q %v", g.Action, g.Why, f.ran)
			}
		})
	}
}

func TestUVRunsOwnOptionsAreReadUpToItsCommand(t *testing.T) {
	for _, c := range []struct {
		argv   string
		layers bool
	}{
		{"uv run --with six srv", true},
		{"uv run --with=six srv", true},
		{"uv run -p 3.12 --with six srv", true},
		{"uv run -qw six srv", true},
		{"uv run -qwsix srv", true},
		{"uv --offline run -q --no-sync --with-requirements r.txt srv", true},
		{"uv run --python 3.12 srv -w 4", false},
		{"uv run srv --with six", false},
		{"uv run -pw srv", false}, // -p takes the rest of its word: python w
		{"uv run -- srv -w 4", false},
		{"uv run - -w", false},                 // a script on stdin
		{"uv run --new-option srv -w 4", true}, // an option not known takes the next word: a miss holds a prune
	} {
		if got := uvLayers(strings.Fields(c.argv)); got != c.layers {
			t.Errorf("%s layers %v, want %v", c.argv, got, c.layers)
		}
	}
}

func TestAProcessProcHidesUnderAHolderIsOneTheLookCannotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every process")
	}
	for _, opt := range []string{"hidepid=invisible", "hidepid=noaccess", "hidepid=2", "hidepid=invisible, a kernel that lists no children"} {
		t.Run(opt, func(t *testing.T) {
			f := newUVLock(t, outside, 4, 6)
			if opt, ok := strings.CutSuffix(opt, ", a kernel that lists no children"); ok {
				o := options(t, f.proc, time.Now(), nil)
				write(t, o.MountInfo, "29 1 259:3 / / rw - ext4 /dev/sda2 rw\n30 29 0:25 / /proc rw - proc proc rw,"+opt+"\n")
				if g := f.run(t, o); g.Done || f.pruned(true) || !strings.Contains(g.Why, "a process /proc hides runs under this one") {
					t.Fatalf("without the kernel's list, a holder's tree may hide a child: %s %q %v", g.Action, g.Why, f.ran)
				}
				return
			}
			withChildren(t, f)
			write(t, filepath.Join(f.proc, "7", "task", "7", "children"), "4300 ") // the server ran a command with sudo
			if opt == "hidepid=noaccess" {                                         // its directory shows, and nothing in it reads
				dir := filepath.Join(f.proc, "4300")
				write(t, filepath.Join(dir, "cmdline"), "sudo\x00worker\x00")
				must(t, os.Chmod(filepath.Join(dir, "cmdline"), 0))
				t.Cleanup(func() { os.Chmod(filepath.Join(dir, "cmdline"), 0o644) })
			}
			o := options(t, f.proc, time.Now(), nil)
			write(t, o.MountInfo, "29 1 259:3 / / rw - ext4 /dev/sda2 rw\n30 29 0:25 / /proc rw - proc proc rw,"+opt+"\n")
			if g := f.run(t, o); g.Done || f.pruned(true) || !strings.Contains(g.Why, "a process /proc hides runs under this one") {
				t.Fatalf("a process /proc hides under a holder is one the look cannot read: %s %q %v", g.Action, g.Why, f.ran)
			}
		})
	}
	f := newUVLock(t, outside, 4, 6) // a /proc that hides nothing shows every child: the look's own rules judge them
	if g := f.run(t, options(t, f.proc, time.Now(), nil)); !g.Done || !f.pruned(true) {
		t.Fatalf("without hidepid no child is hidden: %s %q %v", g.Action, g.Why, f.ran)
	}
}

func TestAHolderAnEarlierSwapLeftOnTheOldFileIsStillAHolder(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every process")
	}
	procs := func(cache, lock string) []fakeProc { // the project's run kept the lock file an earlier swap replaced
		return append(outside(cache, lock)[:2],
			fakeProc{pid: 6, ppid: 1, argv: []string{"uv", "run", "server"}, fds: []string{lock + " (deleted)"}, env: uvEnv},
			fakeProc{pid: 7, ppid: 6, argv: []string{"python", "server.py"}, exe: "/opt/python/bin/python3",
				env: ran("/proj/.venv/bin", "VIRTUAL_ENV=/proj/.venv")},
			fakeProc{pid: 9, ppid: 7, argv: []string{"sudo", "worker"}, env: []string{"PATH=/usr/bin"}})
	}
	f := newUVLock(t, procs, 4)
	write(t, f.lock+" (deleted)", "") // the older file, where the fixture's /proc names it
	old, err := fileID(f.lock + " (deleted)")
	must(t, err)
	b, err := os.ReadFile(filepath.Join(f.proc, "locks"))
	must(t, err)
	write(t, filepath.Join(f.proc, "locks"), string(b)+fmt.Sprintf("8: FLOCK  ADVISORY  READ 6 %02x:%02x:%d 0 EOF\n",
		old.major, old.minor, old.ino))
	must(t, os.Chmod(filepath.Join(f.proc, "9", "fd"), 0))
	t.Cleanup(func() { os.Chmod(filepath.Join(f.proc, "9", "fd"), 0o755) })
	if g := f.run(t, options(t, f.proc, time.Now(), nil)); g.Done || f.pruned(true) || !strings.Contains(g.Why, "cannot read runs under a holder of the lock (pid 9)") {
		t.Fatalf("a process the look cannot read runs under a uv run that holds a lock file capacity replaced: %s %q %v",
			g.Action, g.Why, f.ran)
	}
}
