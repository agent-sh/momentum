package capacity

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Excluded projects are report-only for every rule but models: their paths, a glob that a component of a path
// matches (a renamed checkout, an alias, a lane's scratch folder), and the git repositories whose remote is theirs,
// wherever the checkout or worktree lives.
type excluder struct {
	paths   []string // canonical
	names   []string
	remotes []string // owner/repo, lower case
	m       *Mounts
	git     *gitInfo
}

func newExcluder(c Exclude, m *Mounts, g *gitInfo) *excluder {
	e := &excluder{names: c.Names, m: m, git: g}
	for _, p := range c.Paths {
		e.paths = append(e.paths, m.resolve(p))
	}
	for _, r := range c.Remotes {
		e.remotes = append(e.remotes, strings.ToLower(strings.TrimSuffix(strings.Trim(r, "/"), ".git")))
	}
	return e
}

// byPath reports whether a canonical path is an excluded project's by its place alone, and why.
func (e *excluder) byPath(p string) (string, bool) {
	for _, x := range e.paths {
		if under(p, x) {
			return "under " + x, true
		}
	}
	for _, part := range strings.Split(p, "/") {
		for _, g := range e.names {
			if ok, _ := filepath.Match(g, part); ok {
				return "named " + g, true
			}
		}
	}
	return "", false
}

// excluded reports whether a path found under the targets or worktrees roots is an excluded project's by its place:
// by its canonical path, or as its configured root spells it. A root that is a symlink, or under one, is walked
// where it leads, and a name may be the link's alone.
func (r *runner) excluded(canon string) (string, bool) {
	if why, ok := r.ex.byPath(canon); ok {
		return why, true
	}
	for _, rt := range append(append([]root{}, r.targetRoot...), r.wtRoots...) {
		if rt.shown != rt.canon && under(canon, rt.canon) {
			if why, ok := r.ex.byPath(filepath.Join(rt.shown, strings.TrimPrefix(canon, rt.canon))); ok {
				return why, true
			}
		}
	}
	return "", false
}

// of reports whether a path belongs to an excluded project: by its place, or by the repository it is in (the
// repository's remote, or the place of its main checkout, for a worktree).
func (e *excluder) of(ctx context.Context, p string) (string, bool) {
	if why, ok := e.byPath(p); ok {
		return why, true
	}
	r := e.git.repoOf(ctx, p)
	if r == nil {
		return "", false
	}
	return e.repo(r)
}

// repo reports whether a repository is an excluded project's.
func (e *excluder) repo(r *repo) (string, bool) {
	for _, u := range r.remotes {
		slug := remoteSlug(u)
		for _, x := range e.remotes {
			if slug == x {
				return "a checkout of " + x, true
			}
		}
	}
	if r.common != "" {
		if why, ok := e.byPath(e.m.Canon(filepath.Dir(r.common))); ok {
			return "a worktree of a repository " + why, true
		}
	}
	return "", false
}

// remoteSlug is a git remote's owner/repo in lower case: github.com:owner/repo.git, https://host/owner/repo and
// ssh://git@host/owner/repo all give owner/repo.
func remoteSlug(u string) string {
	u = strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(u), "/"), ".git"))
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
		if j := strings.IndexByte(u, '/'); j >= 0 {
			u = u[j+1:]
		}
	} else if i := strings.IndexByte(u, ':'); i >= 0 {
		u = u[i+1:]
	}
	parts := strings.Split(strings.Trim(u, "/"), "/")
	if len(parts) >= 2 {
		return parts[len(parts)-2] + "/" + parts[len(parts)-1]
	}
	return u
}

// repo is a git work tree: its top level, its common git directory (the main repository's .git) and remotes.
type repo struct {
	top, common string
	remotes     []string
}

// gitInfo answers which repository a path is in, once per work tree.
type gitInfo struct {
	mu    sync.Mutex
	byTop map[string]*repo
}

func newGitInfo() *gitInfo { return &gitInfo{byTop: map[string]*repo{}} }

// repoOf is the work tree a path is in, found by the nearest .git above it, or nil.
func (g *gitInfo) repoOf(ctx context.Context, p string) *repo {
	top := ""
	for cur := p; ; {
		if _, err := os.Lstat(filepath.Join(cur, ".git")); err == nil {
			top = cur
			break
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return nil
		}
		cur = parent
	}
	g.mu.Lock()
	r, ok := g.byTop[top]
	g.mu.Unlock()
	if ok {
		return r
	}
	r = &repo{top: top}
	if out, err := gitOut(ctx, top, "rev-parse", "--path-format=absolute", "--git-common-dir"); err == nil {
		r.common = strings.TrimSpace(out)
	}
	if out, err := gitOut(ctx, top, "remote", "-v"); err == nil {
		seen := map[string]bool{}
		for _, l := range strings.Split(out, "\n") {
			if f := strings.Fields(l); len(f) >= 2 && !seen[f[1]] {
				seen[f[1]] = true
				r.remotes = append(r.remotes, f[1])
			}
		}
	}
	g.mu.Lock()
	g.byTop[top] = r
	g.mu.Unlock()
	return r
}

// gitOut runs git in dir without taking the optional locks (it never refreshes another session's index), without
// prompting, and with a timeout.
func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	return gitRun(ctx, 30*time.Second, dir, args...)
}
