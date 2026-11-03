package loop

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func inputScrubber(t *testing.T, body string) {
	t.Helper()
	previous := Scrubber
	Scrubber = []string{script(t, t.TempDir(), "cleaner", body)}
	t.Cleanup(func() { Scrubber = previous })
}

func goodInputScrubber(t *testing.T) {
	t.Helper()
	inputScrubber(t, `sed -e 's/AKIA[A-Z0-9]*EXAMPLE/[masked]/g' -e 's/marker-7f3a/[masked]/g'`)
}

func TestInputsBatchOrderSeparatorsAndCanary(t *testing.T) {
	goodInputScrubber(t)
	ctx := context.Background()
	input := []string{"first marker-7f3a", "", "third\x1einside", "last\nline"}
	want := []string{"first [masked]", "", "third inside", "last\nline"}
	got, err := RedactAll(ctx, input)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("records %q: %v", got, err)
	}
	if err = CheckScrubber(ctx); err != nil {
		t.Fatal(err)
	}
	if input[2] != "third\x1einside" {
		t.Fatal("caller records changed")
	}
}

func TestInputsBatchFailsClosed(t *testing.T) {
	for _, body := range []string{`tr -d '\036'`, `exit 7`, `cat >/dev/null`} {
		t.Run(body, func(t *testing.T) {
			inputScrubber(t, body)
			if got, err := RedactAll(context.Background(), []string{"one", "two"}); err == nil || got != nil {
				t.Fatalf("accepted incomplete records: %q, %v", got, err)
			}
		})
	}
}

func TestInputsCanaryRefusesBothLeaks(t *testing.T) {
	for _, body := range []string{`cat`, `sed 's/marker-7f3a/[masked]/g'`, `sed 's/AKIA[A-Z0-9]*EXAMPLE/[masked]/g'`, `exit 7`} {
		t.Run(body, func(t *testing.T) {
			inputScrubber(t, body)
			if err := CheckScrubber(context.Background()); err == nil {
				t.Fatal("canary accepted an unsafe cleaner")
			}
		})
	}
	previous := Scrubber
	t.Cleanup(func() { Scrubber = previous })
	for _, scrubber := range [][]string{nil, {filepath.Join(t.TempDir(), "absent")}} {
		Scrubber = scrubber
		if err := CheckScrubber(context.Background()); err == nil {
			t.Fatal("absent cleaner accepted")
		}
	}
}

func TestInputsScrumpRecordsFlagAndBatchLimit(t *testing.T) {
	dir := t.TempDir()
	count := filepath.Join(dir, "calls")
	previous := Scrubber
	t.Cleanup(func() { Scrubber = previous })
	bin := script(t, dir, "scrump", `test "$1" = --profile && test "$2" = text && test "$3" = --records || exit 8
printf 'call\n' >> '`+count+`'
cat`)
	Scrubber = []string{bin, "--profile", "text", "scrub", "-", "-o", "-"}
	in := []string{strings.Repeat("x", ScrubBatchBytes), "last"}
	got, err := RedactAll(context.Background(), in)
	if err != nil || !reflect.DeepEqual(got, in) {
		t.Fatalf("batch result: %v", err)
	}
	calls, err := os.ReadFile(count)
	if err != nil || string(calls) != "call\ncall\n" {
		t.Fatalf("batch calls %q: %v", calls, err)
	}
	if got, err := RedactAll(context.Background(), []string{strings.Repeat("x", ScrubBatchBytes+1)}); err == nil || got != nil {
		t.Fatal("oversized record accepted")
	}
}

func TestInputsMissingScrubberWritesNothing(t *testing.T) {
	previous := Scrubber
	Scrubber = nil
	t.Cleanup(func() { Scrubber = previous })
	dir := t.TempDir()
	home := t.TempDir()
	write(t, filepath.Join(home, ".claude/hooks/check"), "marker-7f3a")
	ctx := context.Background()
	dest := filepath.Join(dir, "copy")
	if _, err := ConfigCopy(ctx, ConfigCopyOptions{Home: home, Destination: dest}); err == nil {
		t.Fatal("configuration copied without a cleaner")
	}
	if _, err := StoreCopy(ctx, StoreCopyOptions{Source: filepath.Join(dir, "absent"), Destination: dest}); err == nil {
		t.Fatal("store copied without a cleaner")
	}
	if _, err := Records(ctx, RecordsOptions{Destination: dest}); err == nil {
		t.Fatal("records copied without a cleaner")
	}
	if _, err := UnitLogs(ctx, RecordsOptions{Destination: dest}); err == nil {
		t.Fatal("units copied without a cleaner")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed preparation wrote files: %d %v", len(entries), err)
	}
}

// A host child that scrubs by itself finds the scrubber: its clean PATH includes the scrubber's own directory, which
// a user's bin directory is not part of.
func TestTheRecordsChildFindsTheScrubber(t *testing.T) {
	was := Scrubber
	t.Cleanup(func() { Scrubber = was })
	dir := t.TempDir()
	Scrubber = []string{script(t, dir, "scrub", "cat")}
	if got := scrubberDir(); got != dir {
		t.Fatalf("scrubber directory %q, want %q", got, dir)
	}
	found := false
	for _, kv := range CleanEnv(nil, scrubberDir()) {
		if strings.HasPrefix(kv, "PATH=") && strings.Contains(kv, dir) {
			found = true
		}
	}
	if !found {
		t.Fatal("the child's PATH lacks the scrubber's directory")
	}
	Scrubber = []string{"no-such-scrubber-marker-7f3a"}
	if scrubberDir() != "" {
		t.Fatal("a missing scrubber has a directory")
	}
}
