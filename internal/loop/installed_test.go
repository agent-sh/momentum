package loop

import (
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// installedCase is a measured case that names a regular file as the one its change is installed in, rebuilt after the
// landing, as the proposal accepted it.
func installedCase(t *testing.T) (*measured, string) {
	t.Helper()
	m := measuredCase(t)
	bin := filepath.Join(t.TempDir(), "tool")
	write(t, bin, "the new build")
	if err := os.Chtimes(bin, m.deployed.Add(time.Minute), m.deployed.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	p, _, _ := m.r.Ship.Load(m.c.ID)
	p.Plan.Installed = bin
	if err := m.r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	return m, bin
}

// inEffectWithin asks the ship tool whether the change is in effect, and fails the test when the question holds it for
// longer than a pipe nobody writes to should: the step opens the installed path outside the sandbox, with the loop's lock.
func inEffectWithin(t *testing.T, m *measured, bin string) (why string, p Package) {
	t.Helper()
	p, _, _ = m.r.Ship.Load(m.c.ID)
	type answer struct {
		why string
		err error
	}
	done := make(chan answer, 1)
	go func() {
		_, why, err := m.r.Ship.InEffect(context.Background(), &p)
		done <- answer{why, err}
	}()
	select {
	case a := <-done:
		if a.err != nil {
			t.Fatal(a.err)
		}
		return a.why, p
	case <-time.After(10 * time.Second):
		go func() { // let the open end, so the test does not leave it behind
			if f, err := os.OpenFile(bin, os.O_WRONLY, 0); err == nil {
				f.Close()
			}
		}()
		<-done
		t.Fatal("the ship tool is held opening the installed path")
	}
	return "", p
}

// The installed file is checked at the proposal and opened later, outside the sandbox, by the step. A path that was a
// regular file and became another thing since is not followed or waited on: a link to a file outside every root is not
// hashed (its hash would reach the package, which a later run reads in the host receipt), a link to a path that is not there
// reads as the link to one that is, a pipe does not hold the step, and a directory is no file. The reason the step gives
// is the same whatever the link points to.
func TestAnInstalledPathSwappedAfterTheProposalIsNotFollowed(t *testing.T) {
	notes := filepath.Join(t.TempDir(), "notes.txt")
	write(t, notes, "marker-7f3a\n")
	noteHash := fmt.Sprintf("%x", sha256.Sum256([]byte("marker-7f3a\n")))
	missing := filepath.Join(t.TempDir(), "absent")
	var reasons []string
	for _, c := range []struct {
		name   string
		swap   func(t *testing.T, bin string)
		reason string
	}{
		{"a link to a file outside every root", func(t *testing.T, bin string) {
			if err := os.Symlink(notes, bin); err != nil {
				t.Fatal(err)
			}
		}, "is a link"},
		{"a link to a path that is not there", func(t *testing.T, bin string) {
			if err := os.Symlink(missing, bin); err != nil {
				t.Fatal(err)
			}
		}, "is a link"},
		{"a pipe nobody writes to", func(t *testing.T, bin string) {
			if err := syscall.Mkfifo(bin, 0o600); err != nil {
				t.Fatal(err)
			}
		}, "is no regular file"},
		{"a directory", func(t *testing.T, bin string) {
			if err := os.Mkdir(bin, 0o700); err != nil {
				t.Fatal(err)
			}
		}, "is no regular file"},
		{"nothing", func(*testing.T, string) {}, "is not there"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, bin := installedCase(t)
			if got := m.measure(t, Options{}); got.Status != "measuring" {
				t.Fatalf("the file as the proposal accepted it: %+v", got)
			}
			before, _, _ := m.r.Ship.Load(m.c.ID)
			if before.InstalledHash == "" || before.InstalledHash == noteHash {
				t.Fatalf("the regular file's own hash is recorded: %q", before.InstalledHash)
			}
			if err := os.Remove(bin); err != nil {
				t.Fatal(err)
			}
			c.swap(t, bin)
			why, p := inEffectWithin(t, m, bin)
			if !strings.Contains(why, c.reason) || strings.Contains(why, "notes.txt") || strings.Contains(why, "marker-7f3a") {
				t.Fatalf("the swapped path: %q, want %q", why, c.reason)
			}
			if p.InstalledHash != before.InstalledHash || p.InstalledOK != before.InstalledOK {
				t.Fatalf("a swapped path changed what the package holds: %+v, was %+v", p, before)
			}
			reasons = append(reasons, why)
			// through the step: it waits with the reason, and the package never holds what the link points to
			got := m.measure(t, Options{})
			if got.Status != "waiting" || !strings.Contains(got.Why, "not in effect") || !strings.Contains(got.Why, c.reason) {
				t.Fatalf("the step on a swapped path: %+v", got)
			}
			if after, _, _ := m.r.Ship.Load(m.c.ID); after.InstalledHash == noteHash {
				t.Fatalf("the package holds the hash of the file the link pointed to: %+v", after)
			}
		})
	}
	if len(reasons) == 5 && reasons[0] != reasons[1] {
		t.Errorf("the reason depends on what a link points to: %q, %q", reasons[0], reasons[1])
	}
}

// openDescriptorsOf counts the descriptors this process holds open on a path.
func openDescriptorsOf(t *testing.T, path string) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("no /proc/self/fd")
	}
	n := 0
	for _, e := range entries {
		if dest, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name())); err == nil && dest == path {
			n++
		}
	}
	return n
}

// The step hashes the installed file and reads its build information from one descriptor: it opens the path once, and a
// path replaced meanwhile is not the file it hashed, whose build information would decide for the other.
func TestTheBuildInformationIsReadFromTheDescriptorTheStepHashed(t *testing.T) {
	m, bin := installedCase(t)
	other := filepath.Join(t.TempDir(), "other")
	write(t, other, "a different file, put at the path while the step reads")
	was := readBuildInfo
	t.Cleanup(func() { readBuildInfo = was })
	var read string
	var open int
	readBuildInfo = func(r io.ReaderAt) (*buildinfo.BuildInfo, error) {
		open = openDescriptorsOf(t, bin)
		if err := os.Rename(other, bin); err != nil { // the path now names another file
			t.Fatal(err)
		}
		b := make([]byte, len("the new build"))
		if _, err := r.ReadAt(b, 0); err != nil {
			t.Fatal(err)
		}
		read = string(b)
		return nil, fmt.Errorf("not a Go binary")
	}
	p, _, _ := m.r.Ship.Load(m.c.ID)
	if _, _, err := m.r.Ship.InEffect(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	if open != 1 {
		t.Fatalf("the installed file is open %d times while its build information is read: it is opened once, and read through that descriptor", open)
	}
	if read != "the new build" {
		t.Fatalf("the build information was read from %q, not from the file the step hashed", read)
	}
	if want := fmt.Sprintf("%x", sha256.Sum256([]byte("the new build"))); p.InstalledHash != want {
		t.Fatalf("hashed %q, want %q", p.InstalledHash, want)
	}
}

// A link is no installed file at the proposal either: the step would refuse it, so the plan does.
func TestAPlanRefusesAnInstalledLink(t *testing.T) {
	m := measuredCase(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "tool")
	write(t, bin, "the new build")
	link := filepath.Join(dir, "link")
	if err := os.Symlink(bin, link); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := m.r.Ship.validPlan(ctx, m.c, goodSignal, 7*24*time.Hour, bin, m.planAt); err != nil && strings.Contains(err.Error(), "installed in") {
		t.Fatalf("a regular file: %v", err)
	}
	if _, err := m.r.Ship.validPlan(ctx, m.c, goodSignal, 7*24*time.Hour, link, m.planAt); err == nil || !strings.Contains(err.Error(), "a link is none") {
		t.Fatalf("a link to a regular file: %v", err)
	}
}
