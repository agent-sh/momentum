package capacity

import "testing"

func TestWhatACallDoesInATree(t *testing.T) {
	const home, tree, above = "/h", "/h/p/shop", "/h/p"
	for _, c := range []struct {
		tool, input, cwd string
		acts             bool
	}{
		// reads, whatever they name
		{"Bash", `{"command":"gh pr checks 9 --repo owner/shop"}`, above, false},
		{"Bash", `{"command":"waitfor --timeout 9m --until 'gh pr checks 9 --repo owner/shop'"}`, above, false},
		{"Bash", `{"command":"cat /h/p/shop/a.go | grep -n x; rg y ~/p/shop"}`, above, false},
		{"Bash", `{"command":"git -C /h/p/shop log --oneline -3 && git -C shop status && git -C shop diff HEAD~1"}`, above, false},
		{"Bash", `{"command":"cd shop && git show HEAD && git stash list"}`, above, false},
		{"Bash", `{"command":"journalctl --user -u shop.service --since today"}`, above, false},
		{"Bash", `{"command":"git clone -q https://example.com/owner/shop"}`, "/h/.cache/x", false},
		{"Bash", `{"command":"sed -n 1,20p shop/a.go; find shop -name '*.go'"}`, above, false},
		{"Read", `{"file_path":"/h/p/shop/a.go"}`, above, false},
		{"Grep", `{"pattern":"x","path":"/h/p/shop"}`, above, false},
		{"Agent", `{"prompt":"never cd into /h/p/shop"}`, above, false},
		{"Write", `{"file_path":"/h/.cache/brief.md","content":"work in /h/p/shop"}`, above, false},
		{"mcp__x__y", `{"repo":"owner/shop"}`, above, false},
		// acts
		{"Bash", `{"command":"ls"}`, "/h/p/shop/sub", true},
		{"Bash", `{"command":"git -C /h/p/shop commit -qm x"}`, above, true},
		{"Bash", `{"command":"git -C ~/p/shop checkout -b topic"}`, above, true},
		{"Bash", `{"command":"cd shop && git reset --hard origin/main"}`, above, true},
		{"Bash", `{"command":"git -C shop stash"}`, above, true},
		{"Bash", `{"command":"git -C shop worktree add ../lane -b lane"}`, above, true},
		{"Bash", `{"command":"git -C other worktree add /h/p/shop/lane"}`, above, true},
		{"Bash", `{"command":"cd shop && go test ./..."}`, above, true},
		{"Bash", `{"command":"go build -C ./shop ./..."}`, above, true},
		{"Bash", `{"command":"echo x > shop/a.txt"}`, above, true},
		{"Bash", `{"command":"sed -i s/a/b/ shop/a.go"}`, above, true},
		{"Bash", `{"command":"rm -rf shop"}`, above, true},
		{"Bash", `{"command":"bash -lc 'cd /h/p/shop && make'"}`, above, true},
		{"Edit", `{"file_path":"/h/p/shop/a.go","old_string":"a","new_string":"b"}`, above, true},
		{"edit", `{"newString":"b","oldString":"a","filePath":"/h/p/shop/a.go"}`, above, true},
		{"exec", `tools.apply_patch("*** Begin Patch\n*** Update File: /h/p/shop/a.go\n@@")`, above, true},
		// fail safe: a call it cannot read, or one the store cut in the tree
		{"mcp__x__y", `{"target":"/h/p/shop/a.go"}`, above, true},
		{"Write", `{"content":"long text that the store cut before its pa…`, above, false},
		{"Write", `{"content":"see /h/p/shop/a.go and the store cut before its pa…`, above, true},
		{"Bash", `{"command":"cd /h/p/shop && git log -1 && git sta…`, above, true},
	} {
		if got := ActsIn(c.tool, c.input, c.cwd, tree, home); got != c.acts {
			t.Errorf("%s %s in %s: acts %v, want %v", c.tool, c.input, c.cwd, got, c.acts)
		}
	}
}
