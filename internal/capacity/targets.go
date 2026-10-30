package capacity

import (
	"bytes"
	"context"
	"io"
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
			Eligible: tree.Newest.Add(idle), root: filepath.Dir(t)}
		f.ino, _ = inodeOf(t)
		if own, foreign := cargoEntries(t); len(foreign) > 0 {
			// Cargo builds its own entries again, not the others (Criterion's saved baselines, a build script's
			// downloads): those stay
			f.parts, f.Bytes = own, 0
			for _, e := range own {
				f.Bytes += Measure(filepath.Join(t, e), nil).Bytes
			}
			f.Detail = "keeps what Cargo does not build: " + strings.Join(foreign[:min(len(foreign), 4)], ", ")
		}
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
	if w, ok := r.excluded(canon); ok {
		return ActReport, "excluded", "an excluded project's: " + w
	}
	known := false
	if rp := r.git.repoOf(ctx, t); rp != nil {
		if w, ok := r.excludedRepo(rp); ok {
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
	sources, complete := depSources(t)
	if !complete {
		return ActReport, "unknown", "it holds more dep-info files than were read, so its projects are not all known"
	}
	for _, s := range sources {
		w, ok := r.excluded(r.m.resolve(s))
		if !ok { // as Cargo wrote it: a build through an alias names the alias
			w, ok = r.ex.byPath(r.m.Canon(filepath.Clean(s)))
		}
		if ok {
			return ActReport, "excluded", "built from an excluded project's sources (" + short(s) + ", " + w + ")"
		}
		if rp := r.git.repoOf(ctx, s); rp != nil {
			if w, ok := r.excludedRepo(rp); ok {
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
// the toolchain, as its dep-info files (<profile>/*.d and <profile>/deps/*.d) name them: the crate's directory (the
// path before its last /src/). complete is false when it holds more than maxDepFiles of them, which are not all read.
func depSources(t string) (out []string, complete bool) {
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
			if read++; read > maxDepFiles {
				return out, false
			}
			b := readHead(filepath.Join(d, e.Name()), maxDepFile+1)
			if len(b) > maxDepFile {
				return out, false // a dep-info file this long is not read whole: its sources are not all known
			}
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
	return out, true
}

// maxDepFiles is the most dep-info files a target dir's ownership is read from, and maxDepFile the longest one.
var maxDepFiles = 20000

const maxDepFile = 4 << 20

// cargoOwn are the entries of a target directory that Cargo (and rust-analyzer's flycheck) makes and makes again.
var cargoOwn = map[string]bool{"CACHEDIR.TAG": true, ".rustc_info.json": true, ".future-incompat-report.json": true,
	".rustdoc_fingerprint.json": true, "debug": true, "release": true, "doc": true, "package": true, "tmp": true}

// cargoEntries splits a target directory's entries into Cargo's own (cargoOwn, flycheck dirs, a custom profile's
// dir, a target triple's) and the others.
func cargoEntries(t string) (own, foreign []string) {
	ents, err := os.ReadDir(t)
	if err != nil {
		return nil, nil
	}
	profile := func(d string) bool {
		for _, s := range []string{".fingerprint", "deps", "build", "incremental"} {
			if st, err := os.Lstat(filepath.Join(d, s)); err == nil && st.IsDir() {
				return true
			}
		}
		return false
	}
	for _, e := range ents {
		n, p := e.Name(), filepath.Join(t, e.Name())
		switch {
		case cargoOwn[n], strings.HasPrefix(n, "flycheck"):
			own = append(own, n)
		case e.IsDir() && profile(p):
			own = append(own, n)
		case e.IsDir() && func() bool { // a target triple: its profiles inside
			for _, d := range listDirs(p) {
				if profile(d) {
					return true
				}
			}
			return false
		}():
			own = append(own, n)
		default:
			foreign = append(foreign, n)
		}
	}
	return own, foreign
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

// readHead is a file's first n bytes, or all of it when it is shorter.
func readHead(p string, n int) []byte {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, int64(n)))
	return b
}
