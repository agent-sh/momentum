package loop

import (
	"os"
	"path/filepath"
	"testing"
)

// A test can leave a directory it made unreadable; the host still removes the tree, links are not followed.
func TestRemoveAllRemovesATreeWithAClosedDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tree")
	closed := filepath.Join(root, "a", "closed")
	if err := os.MkdirAll(filepath.Join(closed, "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(closed, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(root); err == nil && os.Getuid() != 0 {
		t.Skip("this user can remove a closed directory anyway")
	}
	if err := removeAll(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatal("the tree is still there")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("a link was followed")
	}
}

// The loop never changes the CI configuration of any repository, nor the evidence layer of its own.
func TestCheckPathsRefusesCIAndTheEvidenceLayer(t *testing.T) {
	for _, repo := range []string{"agent-sh/other", SelfRepo} {
		for _, path := range []string{".github/workflows/ci.yml", ".gitlab-ci.yml", ".circleci/config.yml", "Jenkinsfile"} {
			if err := CheckPaths(Target{Repo: repo}, []string{path}); err == nil {
				t.Errorf("%s: %s was allowed", repo, path)
			}
		}
	}
	for _, path := range []string{"internal/tracked/check/main.go"} {
		if err := CheckPaths(Target{Repo: SelfRepo}, []string{path}); err == nil {
			t.Errorf("%s was allowed in momentum's own repository", path)
		}
	}
	if err := CheckPaths(Target{Repo: "agent-sh/other"}, []string{"internal/tracked/x.go", "src/main.go"}); err != nil {
		t.Errorf("another repository's ordinary paths were refused: %v", err)
	}
}
