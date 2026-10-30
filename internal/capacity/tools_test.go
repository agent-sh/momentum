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
			pruned := strings.Contains(strings.Join(ran, "\n"), "uv cache prune --ci --force")
			if pruned != c.prunes {
				t.Fatalf("pruned %v, want %v: %v %+v", pruned, c.prunes, ran, res.Rules)
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
		"docker buildx du":   "ID   RECLAIMABLE   SIZE   LAST ACCESSED\nabc   true   1.5GB*   9 days ago\ndef   true   500MB   8 days ago\nghi   false   2GB   1 day ago\n",
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
	if f := got["docker build cache"]; !f.Done || f.Bytes != 2e9 || f.Freed != 1.9e9 {
		t.Errorf("build cache: up to its reclaimable records (2 GB), freed what Docker says: %+v", f)
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

func TestSizesReadAsDockerPrintsThem(t *testing.T) {
	for in, want := range map[string]int64{"1.785GB": 1785000000, "4.096kB": 4096, "512B": 512, "2GiB": 2 << 30, "junk": 0} {
		if got := parseSize(in); got != want {
			t.Errorf("parseSize(%q) = %d, want %d", in, got, want)
		}
	}
}
