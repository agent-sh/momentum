package loop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/momentum/internal/store"
)

// A run's door runs, outside the sandbox, the commands its step allows: momentum's with the loop's variables, in the
// host-only directory, tacit's scrubbed; what the step does not allow runs nothing. The harness types them inside, as
// plain momentum and tacit commands.
func TestARunsDoorRunsItsStepsCommandsOutsideTheSandbox(t *testing.T) {
	was := Scrubber
	t.Cleanup(func() { Scrubber = was })
	Scrubber = []string{script(t, t.TempDir(), "scrub", "sed 's/zz-private-value/****************/g'")}
	st := testStore(t)
	ctx := context.Background()
	outside := t.TempDir()
	log := filepath.Join(outside, "ran")
	momentum := script(t, outside, "momentum", `echo "momentum $* in $PWD run=$MOMENTUM_LOOP_RUN case=$MOMENTUM_LOOP_CASE door=${MOMENTUM_DOOR:-none}" >> `+log+`
echo "momentum answered $1"`)
	tacit := script(t, outside, "tacit", `echo "tacit $*" >> `+log+`
echo "a fact holding zz-private-value"`)
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	harness := script(t, dir, "harness", `momentum evidence show ev:1 >> `+out+` 2>&1
momentum ship propose --title x >> `+out+` 2>&1; echo "propose=$?" >> `+out+`
tacit search lock >> `+out+` 2>&1
echo '`+okResult+`'`)
	r := runnerFor(t, st, []string{harness})
	r.Momentum, r.Tacit = momentum, tacit
	c := openCase(t, st, "failures", "a", 2)
	done, err := r.Once(ctx, Options{Families: []string{"failures"}})
	if err != nil || len(done) != 1 || done[0].Run.Status != store.RunOK {
		t.Fatalf("%+v %v", done, err)
	}
	ran, _ := os.ReadFile(log)
	got, _ := os.ReadFile(out)
	if want := "momentum evidence show ev:1 in " + HostDir(done[0].Run.Dir) + " run=" + done[0].Run.ID + " case=" + c.ID + " door=none"; !strings.Contains(string(ran), want) {
		t.Errorf("the door ran:\n%s\nwant %q", ran, want)
	}
	if strings.Contains(string(ran), "propose") {
		t.Errorf("an investigation's door ran the ship tool:\n%s", ran)
	}
	for _, want := range []string{"momentum answered evidence", "not among the commands of a run's investigate step", "propose=126", "a fact holding ****"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("the harness read %q, want %q", got, want)
		}
	}
	if strings.Contains(string(got), "zz-private-value") {
		t.Errorf("tacit's output reached the run unscrubbed: %s", got)
	}
}
