package loop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// passLeftovers checks what a failed preparation left: nothing bound under /ro, the cache kept, the pass directory removed by close and
// no goroutine of the preparation still running.
func passLeftovers(t *testing.T, r *Runner, cachePath string, goroutines int) {
	t.Helper()
	p := r.pass
	for _, b := range p.binds {
		if strings.HasPrefix(b.Dst, "/ro") {
			t.Fatalf("a failed preparation bound inputs: %+v", p.binds)
		}
	}
	if f := p.cache.Failure(); f != "" {
		t.Fatalf("the cache was dropped: %s", f)
	}
	p.close()
	if _, err := os.Lstat(p.dir); !os.IsNotExist(err) {
		t.Fatalf("the pass directory remains: %v", err)
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("the cache file is gone: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > goroutines {
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines outlived the preparation, %d before it", runtime.NumGoroutine(), goroutines)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The first step that fails is the pass's error: the scrubber fails on the configuration's text only, the other
// copies stop, nothing is bound and the cache stays.
func TestPassInputsFirstFailureIsThePassError(t *testing.T) {
	r, _ := inputRunner(t)
	r.Inputs.ReadDirs = []string{inputRepo(t, t.TempDir(), "example")}
	write(t, filepath.Join(r.Inputs.Home, ".config", "agents", "rules.md"), "config-failure-fixture")
	inputScrubber(t, `in=$(cat; printf x); in=${in%x}
case "$in" in *config-failure-fixture*) exit 1;; esac
printf '%s' "$in" | sed -e 's/AKIA[A-Z0-9]*EXAMPLE/[masked]/g' -e 's/marker-7f3a/[masked]/g'`)
	r.Inputs.ScrubCache = filepath.Join(t.TempDir(), "cache.sqlite")
	goroutines := runtime.NumGoroutine()
	r.pass = &passInputs{}
	err := r.prepareInputs(context.Background())
	if !errors.Is(err, ErrNoScrubber) {
		t.Fatalf("the configuration's scrubber failure is not the pass's error: %v", err)
	}
	passLeftovers(t, r, r.Inputs.ScrubCache, goroutines)
}

// A pass told to stop while the scrubbers run ends with the cancellation: nothing is bound and the cache stays.
func TestPassInputsStopMidPass(t *testing.T) {
	r, _ := inputRunner(t)
	r.Inputs.ReadDirs = []string{inputRepo(t, t.TempDir(), "example")}
	started := filepath.Join(t.TempDir(), "started")
	// canaries pass; every other call says it started and does not end
	inputScrubber(t, `in=$(cat; printf x); in=${in%x}
case "$in" in *AKIA*) ;; *) : > '`+started+`'; exec sleep 30;; esac
printf '%s' "$in" | sed -e 's/AKIA[A-Z0-9]*EXAMPLE/[masked]/g' -e 's/marker-7f3a/[masked]/g'`)
	r.Inputs.ScrubCache = filepath.Join(t.TempDir(), "cache.sqlite")
	goroutines := runtime.NumGoroutine()
	r.pass = &passInputs{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		defer cancel()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(started); err == nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	begin := time.Now()
	err := r.prepareInputs(ctx)
	if !errors.Is(err, context.Canceled) || time.Since(begin) > 20*time.Second {
		t.Fatalf("stopped pass: %v after %s", err, time.Since(begin))
	}
	passLeftovers(t, r, r.Inputs.ScrubCache, goroutines)
}

// A shared child whose answer cannot be split (another number of events, or a failed exit) is run again one event per
// child: every record is its own child's.
func TestRecordsSharedChildThatCannotBeSplit(t *testing.T) {
	for _, tc := range []struct{ name, shared string }{
		{"part count", `first=$1; sep=''; for id in "$@"; do [ "$id" = "$first" ] && continue; printf "$sep"; printf 'record %s\n' "$id"; sep='\036'; done`},
		{"exit", `sep=''; for id in "$@"; do printf "$sep"; printf 'shared %s\n' "$id"; sep='\036'; done; exit 3`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			goodInputScrubber(t)
			at := time.Date(2025, 4, 10, 12, 0, 0, 0, time.UTC)
			var events []store.Event
			for i := range 12 {
				events = append(events, store.Event{Source: "fixture", SourceKey: strconv.Itoa(i), TS: at, Kind: "record"})
			}
			o := recordFixture(t, events, `if test "$3" = --separate; then
	shift 6
	`+tc.shared+`
	exit 0
fi
printf 'record %s\n' "$6"`)
			o.Neighbours = -1
			for _, e := range events {
				o.IDs = append(o.IDs, e.ID())
			}
			m, err := Records(context.Background(), o)
			if err != nil || m.Written != len(events) || m.Withheld != 0 {
				t.Fatalf("manifest %+v %v", m, err)
			}
			for i, file := range m.Files {
				if file.Event != events[i].ID() || readText(t, filepath.Join(o.Destination, file.Name)) != "record "+events[i].ID()+"\n" {
					t.Fatalf("record %d is not its own child's output: %+v", i, file)
				}
			}
		})
	}
}
