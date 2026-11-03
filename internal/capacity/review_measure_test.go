package capacity

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

func TestPartialCapacityScansAreIncomplete(t *testing.T) {
	for _, cause := range []string{"rule", "disk", "rule-and-disk", "models-root"} {
		t.Run(cause, func(t *testing.T) {
			ctx := context.Background()
			st := openStore(t)
			now := time.Now().Truncate(time.Millisecond)
			cfg := Config{}.Normal()
			opt := options(t, procRoot(t), now, nil)
			opt.Now, opt.Rules = true, []string{RuleUV}
			cache := t.TempDir()
			opt.Tools = fakeTools{answers: map[string]string{"uv cache dir": cache, "uv cache size": "0", "docker": "ERR"}}
			if cause == "rule" || cause == "rule-and-disk" {
				cfg.Live = []string{RuleUV}
				opt.Tools = fakeTools{answers: map[string]string{"uv cache dir": "ERR-INUSE", "docker": "ERR"}}
			}
			if cause == "disk" || cause == "rule-and-disk" {
				cfg.Disks = []string{filepath.Join(t.TempDir(), "absent")}
			}
			if cause == "models-root" {
				root := filepath.Join(t.TempDir(), "not-a-directory")
				write(t, root, "ordinary file")
				cfg.Models.Roots, opt.Rules = []string{root}, []string{RuleModels}
			}
			for _, dry := range []bool{true, false} {
				opt.DryRun = dry
				res, err := Run(ctx, cfg, st, opt)
				if err != nil || res == nil {
					t.Fatalf("partial scan should return its observations: %+v %v", res, err)
				}
				if cause == "rule-and-disk" && (res.Rules[0].Err == "" || res.Disks[0].Err == "") {
					t.Fatal("fixture did not reproduce both observation errors")
				}
				read, err := st.ReadThrough(ctx, store.Capacity)
				must(t, err)
				gap, err := st.Incomplete(ctx, store.Capacity, "", now.Add(-time.Hour), now.Add(time.Hour))
				must(t, err)
				if !read.IsZero() || (dry && !gap.IsZero()) || (!dry && !gap.Equal(now)) {
					t.Fatalf("dry %t: partial scan certified a read or lost its gap: read %v, incomplete %v", dry, read, gap)
				}
			}
		})
	}
}
