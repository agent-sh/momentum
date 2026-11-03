package loop

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestInputsConfigurationReadOnlyUnderPrivateUmask(t *testing.T) {
	goodInputScrubber(t)
	home := t.TempDir()
	write(t, filepath.Join(home, ".claude/hooks/file"), "plain\n")
	dst := filepath.Join(t.TempDir(), "copy")
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	if _, err := ConfigCopy(context.Background(), ConfigCopyOptions{Home: home, Destination: dst}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeInputTree(dst) })
	configTree(t, dst)
}

func TestInputsManifestNamesCannotLeakMarkers(t *testing.T) {
	goodInputScrubber(t)
	home := t.TempDir()
	write(t, filepath.Join(home, ".claude/hooks/credential-marker-7f3a.txt"), "plain")
	write(t, filepath.Join(home, ".claude/hooks/name-marker-7f3a.txt"), "plain")
	dst := filepath.Join(t.TempDir(), "config")
	m, err := ConfigCopy(context.Background(), ConfigCopyOptions{Home: home, Destination: dst})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeInputTree(dst) })
	b, err := json.Marshal(m)
	if err != nil || strings.Contains(string(b), "marker-7f3a") || len(m.Files) != 0 || m.SkippedByReason["path needs scrubbing"] != 1 {
		t.Fatalf("configuration name leaked: files %d, skips %v, error %v", len(m.Files), m.SkippedByReason, err)
	}
	o := recordFixture(t, nil, `printf 'plain\n'`)
	o.Units = []string{"unit-marker-7f3a.service"}
	units, err := UnitLogs(context.Background(), o)
	if err != nil || units.Written != 0 || units.Skipped != 1 {
		t.Fatal("unit name bypassed scrubbing")
	}
	src := inputRepo(t, t.TempDir(), "repo-marker-7f3a")
	clones, err := SnapshotRepos(context.Background(), snapOptions(t.TempDir(), src))
	b, marshalErr := json.Marshal(clones)
	if err != nil || marshalErr != nil || len(clones) != 1 || clones[0].Error == "" || strings.Contains(string(b), "marker-7f3a") {
		t.Fatal("repository name bypassed scrubbing")
	}
}

func TestInputsDefaultBranchOnlyInRemoteTrackingRefs(t *testing.T) {
	goodInputScrubber(t)
	parent := t.TempDir()
	src := inputRepo(t, parent, "sample")
	inputTestGit(t, src, nil, "branch", "-m", "main", "stable")
	inputTestGit(t, src, nil, "checkout", "stable")
	write(t, filepath.Join(src, "file"), "remote branch\n")
	inputTestGit(t, src, nil, "add", "file")
	inputTestGit(t, src, nil, "commit", "-m", "remote default")
	commit := inputTestGit(t, src, nil, "rev-parse", "HEAD")
	inputTestGit(t, src, nil, "update-ref", "refs/remotes/origin/stable", commit)
	inputTestGit(t, src, nil, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/stable")
	inputTestGit(t, src, nil, "checkout", "feature")
	inputTestGit(t, src, nil, "branch", "-D", "stable")
	got, err := SnapshotRepos(context.Background(), snapOptions(parent, src))
	if err != nil || len(got) != 1 || got[0].Error != "" || got[0].Branch != "stable" || got[0].Commit != commit {
		t.Fatalf("remote default branch metadata: %v %v", got, err)
	}
	text, err := os.ReadFile(filepath.Join(got[0].Path, "file"))
	if err != nil || string(text) != "remote branch\n" {
		t.Fatal("snapshot omitted the remote default branch's files")
	}
}
