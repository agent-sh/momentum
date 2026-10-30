package capacity

import (
	"context"
	"strings"
	"testing"
	"time"

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
		{"a uv run past its sync", []fakeProc{{pid: 4, ppid: 1, argv: []string{"uv", "run", "server"}},
			{pid: 5, ppid: 4, argv: []string{"python", "server.py"}}}, false, ActReport},
		{"a uvx tool server runs", []fakeProc{{pid: 4, ppid: 1, argv: []string{"uvx", "mcp-x"}}, {pid: 5, ppid: 4, argv: []string{"mcp-x"}}}, false, ActReport},
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
				t.Fatalf("a prune never passes uv's lock: %v", ran)
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
		after string
		notes int
	}{{"25000000000", 1}, {"40000000000", 0}} {
		pruned := false
		cfg := Config{Live: []string{RuleUV}}.Normal()
		o := options(t, procRoot(t), time.Now(), nil)
		o.Tools, o.Rules = shrinking{uvTools("40000000000", nil), c.after, &pruned}, []string{RuleUV}
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
		"docker buildx du":   duRecords,
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
	evs, err := st.Events(context.Background(), store.Query{Source: store.Capacity, Kinds: []string{KindPrune}})
	if err != nil || len(evs) != 2 {
		t.Errorf("each prune is an event: %d %v", len(evs), err)
	}
}

// duRecords is `docker buildx du --format json`: of the build cache, a prune limited to 168 hours removes the
// 1.5 GB record last used 9 days ago and the 500 MB one never used, and nothing else.
const duRecords = `{"ID":"old","LastUsedAt":"9 days ago","Reclaimable":true,"Shared":false,"Size":"1.5GB","Type":"regular"}
{"ID":"never","LastUsedAt":"","Reclaimable":true,"Shared":false,"Size":"500MB","Type":"exec.cachemount"}
{"ID":"shared","LastUsedAt":"12 days ago","Reclaimable":true,"Shared":true,"Size":"30GB","Type":"regular"}
{"ID":"recent","LastUsedAt":"6 days ago","Reclaimable":true,"Shared":false,"Size":"20GB","Type":"regular"}
{"ID":"inuse","LastUsedAt":"3 weeks ago","Reclaimable":false,"Shared":false,"Size":"4GB","Type":"regular"}
{"ID":"internal","LastUsedAt":"2 months ago","Reclaimable":true,"Shared":false,"Size":"1GB","Type":"internal"}
{"ID":"frontend","LastUsedAt":"2 months ago","Reclaimable":true,"Shared":false,"Size":"1GB","Type":"frontend"}
{"ID":"odd","LastUsedAt":"a while ago","Reclaimable":true,"Shared":false,"Size":"7GB","Type":"regular"}
`

func TestAPruneThatCanFreeNothingDoesNotRunAndOneThatFreedNothingIsNoNote(t *testing.T) {
	for _, c := range []struct {
		name, du, said string
		runs           bool
	}{
		{"only shared and recent records", `{"LastUsedAt":"12 days ago","Reclaimable":true,"Shared":true,"Size":"30GB","Type":"regular"}
{"LastUsedAt":"About an hour ago","Reclaimable":true,"Shared":false,"Size":"20GB","Type":"regular"}`, "", false},
		{"Docker removes nothing", duRecords, "Total:\t0B\n", true},
	} {
		var ran []string
		tools := fakeTools{ran: &ran, answers: map[string]string{"uv": "ERR", "docker info": "29.0.0\n", "docker buildx du": c.du,
			"docker image ls": "", "docker system df": "[]", "docker builder": c.said}}
		cfg := Config{Live: []string{RuleDocker}}.Normal()
		o := options(t, procRoot(t), time.Now(), nil)
		o.Tools, o.Rules = tools, []string{RuleDocker}
		st := openStore(t)
		ctx := context.Background()
		if _, err := Run(ctx, cfg, st, o); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(strings.Join(ran, "\n"), "builder prune"); got != c.runs {
			t.Errorf("%s: pruned %v", c.name, got)
		}
		evs, err := st.Events(ctx, store.Query{Source: store.Capacity, Kinds: []string{KindPrune}})
		if err != nil || len(evs) != map[bool]int{true: 1}[c.runs] {
			t.Errorf("%s: a prune that ran is an event: %d %v", c.name, len(evs), err)
		}
		ns, err := st.Notes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range ns {
			if n.Kind == store.NoteDecided {
				t.Errorf("%s: nothing freed, no decided note: %s", c.name, n.Title)
			}
		}
	}
}

func TestAgesReadAsDockerPrintsThem(t *testing.T) {
	day := 24 * time.Hour
	for in, want := range map[string]time.Duration{"Less than a second ago": 0, "About a minute ago": time.Minute,
		"45 minutes ago": 45 * time.Minute, "About an hour ago": time.Hour, "47 hours ago": 47 * time.Hour,
		"7 days ago": 7 * day, "2 weeks ago": 14 * day, "3 months ago": 90 * day, "2 years ago": 730 * day} {
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
