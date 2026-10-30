package capacity

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The targets rule. A Cargo target directory (Cargo's CACHEDIR.TAG and its .rustc_info.json) that has had no write
// for the idle time, that no process uses, that no job names and that git does not track (git ignores it, or it is
// in no repository) is deleted: Cargo builds it again. Its project must be known and not excluded: the repository
// it is in, or for one outside any repository, the repositories of the sources it was built from, which its
// dep-info files name. A target dir of an excluded project, of an unknown one, or one git does not ignore is only
// reported.

func (r *runner) targets(ctx context.Context, paths []string) ([]Finding, error) {
	idle := time.Duration(r.cfg.Targets.IdleHours * float64(time.Hour))
	cutoff := r.now.Add(-idle)
	var out []Finding
	for _, t := range paths {
		if newerThan(t, cutoff) { // a build in the idle time: the cheap look at its directories says so
			continue
		}
		tree := Measure(t, nil)
		if tree.Newest.After(cutoff) || tree.Bytes < int64(r.cfg.MinMiB*(1<<20)) {
			continue
		}
		canon := r.m.Canon(t)
		f := Finding{Rule: RuleTargets, Path: t, canon: canon, Bytes: tree.Bytes, Newest: tree.Newest, Idle: tree.Newest,
			Eligible: tree.Newest.Add(idle)}
		switch u, inUse := r.sample.InUse(canon); {
		case inUse:
			f.Action, f.Why = ActKeep, "in use now: "+useText(u)
		default:
			if j, ok := r.named.Protects(canon); ok {
				f.Action, f.Why = ActKeep, "named by "+j
				break
			}
			f.Action, f.class, f.Why = r.targetOwner(ctx, t, canon)
		}
		out = append(out, f)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Bytes > out[j].Bytes })
	return out, nil
}

// targetOwner decides on an idle target dir by its project.
func (r *runner) targetOwner(ctx context.Context, t, canon string) (action, class, why string) {
	if w, ok := r.ex.byPath(canon); ok {
		return ActReport, "excluded", "an excluded project's: " + w
	}
	known := false
	if rp := r.git.repoOf(ctx, t); rp != nil {
		if w, ok := r.ex.repo(rp); ok {
			return ActReport, "excluded", "an excluded project's: " + w
		}
		ignored, err := gitOK(ctx, rp.top, "check-ignore", "-q", t)
		if err != nil {
			return ActReport, "unknown", "git could not say whether it ignores it: " + err.Error()
		}
		if !ignored {
			return ActReport, "tracked", "git in " + short(rp.top) + " does not ignore it"
		}
		known = true
	}
	for _, s := range depSources(t) {
		if w, ok := r.ex.byPath(r.m.resolve(s)); ok {
			return ActReport, "excluded", "built from an excluded project's sources (" + short(s) + ", " + w + ")"
		}
		if rp := r.git.repoOf(ctx, s); rp != nil {
			if w, ok := r.ex.repo(rp); ok {
				return ActReport, "excluded", "built from an excluded project's sources (" + short(s) + ", " + w + ")"
			}
			known = true
		}
	}
	if !known {
		return ActReport, "unknown", "its project is unknown: it is in no repository, and no source it was built from is in one"
	}
	return ActDelete, "", ""
}

// depSources are the source directories a target dir was built from, outside it and outside Cargo's registry and
// the toolchain, as its dep-info files (<profile>/*.d and <profile>/deps/*.d) name them: the crate's directory
// (the path before /src/), at most 20, and at most maxDepFiles files read.
func depSources(t string) []string {
	const maxDepFiles, maxSources = 4000, 20
	var dirs []string
	for _, a := range listDirs(t) {
		dirs = append(dirs, a, filepath.Join(a, "deps"))
		for _, b := range listDirs(a) { // a target triple's profiles
			if filepath.Base(b) != "deps" {
				dirs = append(dirs, b, filepath.Join(b, "deps"))
			}
		}
	}
	seen := map[string]bool{}
	var out []string
	read := 0
	for _, d := range dirs {
		ents, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if !strings.HasSuffix(e.Name(), ".d") || !e.Type().IsRegular() {
				continue
			}
			if read++; read > maxDepFiles || len(out) >= maxSources {
				return out
			}
			b := readHead(filepath.Join(d, e.Name()), 64<<10)
			for _, w := range bytes.Fields(b) {
				p := strings.TrimSuffix(string(w), ":")
				if !filepath.IsAbs(p) || !strings.HasSuffix(p, ".rs") || under(p, t) || toolchainPath(p) {
					continue
				}
				dir := filepath.Dir(p)
				if i := strings.LastIndex(p, "/src/"); i > 0 { // the crate's own src: a checkout may be under a src too
					dir = p[:i]
				}
				if !seen[dir] {
					seen[dir] = true
					out = append(out, dir)
				}
			}
		}
	}
	return out
}

// toolchainPath reports whether a source path is Cargo's registry or git cache, or the toolchain's own library.
func toolchainPath(p string) bool {
	for _, s := range []string{"/.cargo/registry/", "/.cargo/git/", "/registry/src/", "/git/checkouts/", "/rustlib/", "/.rustup/"} {
		if strings.Contains(p, s) {
			return true
		}
	}
	return false
}

func listDirs(d string) []string {
	ents, err := os.ReadDir(d)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, filepath.Join(d, e.Name()))
		}
	}
	return out
}

// readHead is a file's first n bytes.
func readHead(p string, n int) []byte {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	b := make([]byte, n)
	k, _ := f.Read(b)
	return b[:k]
}
