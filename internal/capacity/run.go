package capacity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// Finding is one item a rule looked at and acted on, would act on, or could only report.
type Finding struct {
	Rule   string    `json:"rule"`
	Path   string    `json:"path"`
	Bytes  int64     `json:"bytes"`
	Newest time.Time `json:"newest,omitzero"` // its newest write
	Seen   time.Time `json:"seen,omitzero"`   // the last time the sampler saw it in use
	Idle   time.Time `json:"idle_since,omitzero"`
	// Action is delete (it qualifies; Done says whether this run deleted it), pending (it will qualify at
	// Eligible if nothing changes), keep (it would qualify but for Why) or report (only reported: Why says why).
	Action       string    `json:"action"`
	Why          string    `json:"why,omitempty"`
	Eligible     time.Time `json:"eligible,omitzero"`
	Done         bool      `json:"done,omitempty"`
	Freed        int64     `json:"freed,omitempty"` // measured on its disk when it was deleted
	Err          string    `json:"error,omitempty"`
	Detail       string    `json:"detail,omitempty"`
	canon        string    // the canonical path, which every comparison uses (Mounts)
	repo, branch string    // a worktree's common git directory and its branch
	blobs        []string  // a model's shared-store blobs that go with it, canonical
	linked       []string  // every shared-store blob a model links, canonical
	class        string    // why a report-only finding is reported, as a word: its note's group
}

// The actions a finding can have.
const (
	ActDelete  = "delete"
	ActPending = "pending"
	ActKeep    = "keep"
	ActReport  = "report"
)

// RuleResult is what one rule did on a run.
type RuleResult struct {
	Rule     string        `json:"rule"`
	Live     bool          `json:"live"`
	Ran      bool          `json:"ran"`
	Why      string        `json:"why,omitempty"` // why it did not run
	Took     time.Duration `json:"-"`
	TookMS   int64         `json:"took_ms"`
	Findings []Finding     `json:"findings"`
	Notes    []string      `json:"notes,omitempty"`
	Err      string        `json:"error,omitempty"`
}

// DiskState is one disk at this run, and how fast it fills.
type DiskState struct {
	Disk      string        `json:"disk"`
	Size      int64         `json:"size"`
	Used      int64         `json:"used"`
	Avail     int64         `json:"avail"`
	Rate      float64       `json:"gib_per_hour"` // over the window; 0 without two samples far enough apart
	Window    time.Duration `json:"-"`
	FullIn    time.Duration `json:"-"` // until Full percent at this rate
	WindowMS  int64         `json:"window_ms"`
	FullInMS  int64         `json:"full_in_ms,omitempty"`
	Fast      bool          `json:"fast,omitempty"`
	Writers   []Writer      `json:"writers,omitempty"`
	Err       string        `json:"error,omitempty"`
	pctFull   float64
	mountPath string
}

// Writer is a process writing fast while a disk filled.
type Writer struct {
	PID     int     `json:"pid"`
	Comm    string  `json:"comm"`
	Cwd     string  `json:"cwd"`
	Rate    float64 `json:"mib_per_s"`
	OnDisk  bool    `json:"on_disk"` // its working directory is on the disk
	Session string  `json:"session,omitempty"`
}

// Result is one run.
type Result struct {
	At         time.Time     `json:"at"`
	DryRun     bool          `json:"dry_run"`
	SampleTook time.Duration `json:"-"`
	SampleMS   int64         `json:"sample_ms"`
	Procs      int           `json:"procs"`
	Unreadable int           `json:"unreadable"`
	Since      time.Time     `json:"sampler_since,omitzero"` // where the sampler's record starts
	Disks      []DiskState   `json:"disks"`
	Rules      []RuleResult  `json:"rules"`
	Problems   []string      `json:"problems,omitempty"`
}

// Session names the agent session behind a process, for the writers of a fast fill.
type Session struct {
	Key, Name, Tool string
	Cwd             string // where it works
}

// Options are how a run runs.
type Options struct {
	DryRun    bool     // change nothing, record nothing: list what each rule would do
	Now       bool     // every rule scans now, whatever its cadence
	Rules     []string // only these rules; empty is all
	ProcRoot  string   // default /proc
	MountInfo string   // default /proc/self/mountinfo
	Jobs      Jobs     // default SystemJobs
	// Sessions maps a process to the agent session it runs under; nil names none.
	Sessions func(ctx context.Context) map[int]Session
	// Tools runs uv and docker; nil runs the real ones.
	Tools Tools
	// WriterWait is how long the writers of a fast fill are measured over; default 5 s.
	WriterWait time.Duration
	// Clock is the time; default time.Now. Writes reads each process's written bytes; default Writes.
	Clock  func() time.Time
	Writes func() map[int]int64
}

type runner struct {
	cfg   Config
	opt   Options
	st    *store.Store
	now   time.Time
	m     *Mounts
	git   *gitInfo
	ex    *excluder
	tools Tools

	sample       *Sample
	fresh        *Sample // taken just before the deletions
	named        *Named
	namedErr     error
	sessions     map[int]Session // the live agent sessions, read when first needed
	seen         map[string]store.Seen
	samplerStart time.Time

	modelRoots []root          // deepest first
	modelSet   map[string]bool // canonical
	stores     map[string]bool // canonical model roots with a Hugging Face shared blob store
	keep       map[string]bool // canonical
	targetRoot []root
	wtRoots    []root
	res        *Result
}

// root is a configured root: as configured, and canonical.
type root struct{ shown, canon string }

func shownOf(rs []root) []string {
	out := make([]string, len(rs))
	for i, x := range rs {
		out[i] = x.shown
	}
	return out
}

// Run is one capacity run: the sample of the processes, each disk's use, then each rule that is due.
func Run(ctx context.Context, cfg Config, st *store.Store, opt Options) (*Result, error) {
	if opt.ProcRoot == "" {
		opt.ProcRoot = "/proc"
	}
	if opt.MountInfo == "" {
		opt.MountInfo = "/proc/self/mountinfo"
	}
	if opt.Jobs == nil {
		opt.Jobs = SystemJobs{}
	}
	if opt.WriterWait <= 0 {
		opt.WriterWait = 5 * time.Second
	}
	if opt.Clock == nil {
		opt.Clock = time.Now
	}
	if opt.Writes == nil {
		root := opt.ProcRoot
		opt.Writes = func() map[int]int64 { w, _ := Writes(root); return w }
	}
	r := &runner{cfg: cfg, opt: opt, st: st, now: opt.Clock(), git: newGitInfo(), tools: opt.Tools}
	if r.tools == nil {
		r.tools = execTools{uv: cfg.UV.Path, docker: cfg.Docker.Path}
	}
	r.m = ReadMounts(opt.MountInfo)
	r.ex = newExcluder(cfg.Exclude, r.m, r.git)
	r.res = &Result{At: r.now, DryRun: opt.DryRun}
	r.roots()

	r.sample = TakeSample(opt.ProcRoot, r.m, r.watched)
	r.res.SampleTook, r.res.Procs, r.res.Unreadable = r.sample.Took, r.sample.Procs, r.sample.Unreadable
	r.res.SampleMS = r.sample.Took.Milliseconds()
	disks := r.diskSamples()
	var err error
	if r.seen, err = r.seenPaths(ctx); err != nil {
		return nil, err
	}
	seen := r.sampleSeen()
	if !opt.DryRun && st == nil {
		return nil, errors.New("a run that records needs the store")
	}
	if !opt.DryRun {
		samples := make([]store.DiskSample, 0, len(disks))
		for _, d := range disks {
			if d.Err == "" {
				samples = append(samples, store.DiskSample{Disk: d.Disk, At: r.now, Size: d.Size, Used: d.Used, Avail: d.Avail})
			}
		}
		if err := st.PutCapacityRun(ctx, store.CapacityRun{At: r.now, Sample: r.sample.Took, Procs: r.sample.Procs,
			Unreadable: r.sample.Unreadable}, samples, seen); err != nil {
			return nil, err
		}
	}
	for _, v := range seen { // this run's sightings count at once
		old := r.seen[v.Path]
		if v.Last.After(old.Last) {
			old.Path, old.Last, old.How, old.Proc = v.Path, v.Last, v.How, v.Proc
			if old.First.IsZero() {
				old.First = v.Last
			}
			r.seen[v.Path] = old
		}
	}
	if st != nil {
		if r.samplerStart, err = st.SamplerStart(ctx); err != nil {
			return nil, err
		}
	}
	if r.samplerStart.IsZero() {
		r.samplerStart = r.now // a dry run before any run: the record would start now
	}
	r.res.Since = r.samplerStart

	fast := r.disk(ctx, disks)
	due := map[string]bool{}
	for _, rule := range Rules {
		if len(opt.Rules) > 0 && !contains(opt.Rules, rule) {
			continue
		}
		rr := RuleResult{Rule: rule, Live: cfg.IsLive(rule) && !opt.DryRun}
		switch ok, why := r.due(ctx, rule, fast); {
		case !cfg.configured(rule):
			rr.Why = "not configured"
		case !ok:
			rr.Why = why
		default:
			due[rule] = true
			rr.Ran = true
		}
		r.res.Rules = append(r.res.Rules, rr)
	}
	if len(due) > 0 {
		r.named, r.namedErr = LoadNamed(ctx, opt.Jobs, cfg.Names, r.m)
		if r.namedErr != nil {
			r.res.Problems = append(r.res.Problems, "the scheduled jobs could not all be read, so nothing is deleted this run: "+r.namedErr.Error())
		}
	}
	var found Found
	if due[RuleTargets] || due[RuleWorktrees] {
		found = Discover(append(shownOf(r.targetRoot), shownOf(r.wtRoots)...),
			func(p string) bool { return r.inTargetRoots(r.m.Canon(p)) }, func(p string) bool { return r.inWorktreeRoots(r.m.Canon(p)) }, nil)
	}
	for i := range r.res.Rules {
		rr := &r.res.Rules[i]
		if !rr.Ran {
			continue
		}
		start := time.Now()
		var err error
		switch rr.Rule {
		case RuleModels:
			rr.Findings, err = r.models(ctx)
		case RuleTargets:
			rr.Findings, err = r.targets(ctx, found.Targets)
			rr.Notes = append(rr.Notes, fmt.Sprintf("walked %d directories in %s", found.Dirs, found.Took.Round(time.Millisecond)))
		case RuleWorktrees:
			rr.Findings, err = r.worktrees(ctx, found.Worktrees)
		case RuleUV:
			rr.Findings, rr.Notes, err = r.uv(ctx, rr.Live)
		case RuleDocker:
			rr.Findings, rr.Notes, err = r.docker(ctx, rr.Live)
		}
		if err != nil {
			rr.Err = err.Error()
		}
		rr.Took = time.Since(start)
		rr.TookMS = rr.Took.Milliseconds()
	}
	r.act(ctx)
	if !opt.DryRun {
		for _, rr := range r.res.Rules {
			if rr.Ran && rr.Err == "" {
				if err := st.SetMark(ctx, "scan:"+rr.Rule, r.now); err != nil {
					return r.res, err
				}
			}
		}
		if err := r.record(ctx); err != nil {
			return r.res, err
		}
	}
	return r.res, nil
}

// roots makes the configured roots canonical.
func (r *runner) roots() {
	r.modelSet, r.stores, r.keep = map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, p := range r.cfg.Models.Roots {
		c := r.m.resolve(p)
		if !r.modelSet[c] {
			r.modelSet[c] = true
			r.modelRoots = append(r.modelRoots, root{shown: filepath.Clean(p), canon: c})
		}
		if _, err := os.Stat(filepath.Join(p, "blobs", hfStoreMarker)); err == nil {
			r.stores[c] = true
		}
	}
	sort.SliceStable(r.modelRoots, func(i, j int) bool { return len(r.modelRoots[i].canon) > len(r.modelRoots[j].canon) })
	for _, p := range r.cfg.Models.Keep {
		r.keep[r.m.resolve(p)] = true
	}
	for _, p := range r.cfg.Targets.Roots {
		r.targetRoot = append(r.targetRoot, root{shown: filepath.Clean(p), canon: r.m.resolve(p)})
	}
	for _, g := range r.cfg.Worktrees.Roots {
		ms, _ := filepath.Glob(g)
		for _, p := range ms {
			if st, err := os.Stat(p); err == nil && st.IsDir() {
				r.wtRoots = append(r.wtRoots, root{shown: filepath.Clean(p), canon: r.m.resolve(p)})
			}
		}
	}
}

// inRoots reports whether a canonical path is under one of the roots.
func inRoots(p string, rs []root) bool {
	for _, x := range rs {
		if under(p, x.canon) {
			return true
		}
	}
	return false
}

func (r *runner) inModelRoots(p string) bool    { return inRoots(p, r.modelRoots) }
func (r *runner) inTargetRoots(p string) bool   { return inRoots(p, r.targetRoot) }
func (r *runner) inWorktreeRoots(p string) bool { return inRoots(p, r.wtRoots) }

// watched says which paths the sampler keeps: those under any rule's roots.
func (r *runner) watched(p string) bool {
	return r.inModelRoots(p) || r.inTargetRoots(p) || r.inWorktreeRoots(p)
}

// due reports whether a rule scans on this run: always on a dry run, with --now or while a disk fills fast;
// otherwise once its cadence has passed since its last scan.
func (r *runner) due(ctx context.Context, rule string, fast bool) (bool, string) {
	if r.opt.DryRun || r.opt.Now || fast {
		return true, ""
	}
	last, err := r.mark(ctx, "scan:"+rule)
	if err != nil {
		return false, err.Error()
	}
	if every := r.cfg.every(rule); !last.IsZero() && r.now.Sub(last) < every {
		return false, fmt.Sprintf("scanned %s ago; scans every %s", r.now.Sub(last).Round(time.Minute), every)
	}
	return true, ""
}

// The run's reads of the store. A dry run may have no store (none yet, or one this build has not migrated): it
// reads as one with no history.

func (r *runner) seenPaths(ctx context.Context) (map[string]store.Seen, error) {
	if r.st == nil {
		return map[string]store.Seen{}, nil
	}
	return r.st.SeenPaths(ctx)
}

func (r *runner) mark(ctx context.Context, m string) (time.Time, error) {
	if r.st == nil {
		return time.Time{}, nil
	}
	return r.st.Mark(ctx, m)
}

func (r *runner) diskHistory(ctx context.Context, disk string, since time.Time) ([]store.DiskSample, error) {
	if r.st == nil {
		return nil, nil
	}
	return r.st.DiskSamples(ctx, disk, since)
}

// diskSamples reads each configured disk's use.
func (r *runner) diskSamples() []DiskState {
	var out []DiskState
	for _, d := range r.cfg.Disks {
		var s syscall.Statfs_t
		ds := DiskState{Disk: d, mountPath: d}
		if err := syscall.Statfs(d, &s); err != nil {
			ds.Err = err.Error()
		} else {
			bs := int64(s.Bsize)
			ds.Size, ds.Avail = int64(s.Blocks)*bs, int64(s.Bavail)*bs
			ds.Used = int64(s.Blocks-s.Bfree) * bs
		}
		out = append(out, ds)
	}
	return out
}

// act deletes what the live rules found, after one fresh look at the processes: a build or a load that began
// since the walk keeps its item. Nothing is deleted when the scheduled jobs could not all be read.
func (r *runner) act(ctx context.Context) {
	var todo []*Finding
	for i := range r.res.Rules {
		rr := &r.res.Rules[i]
		if !rr.Live || rr.Rule == RuleUV || rr.Rule == RuleDocker {
			continue
		}
		for j := range rr.Findings {
			if rr.Findings[j].Action == ActDelete {
				todo = append(todo, &rr.Findings[j])
			}
		}
	}
	if len(todo) == 0 {
		return
	}
	if r.namedErr != nil {
		for _, f := range todo {
			f.Action, f.Why = ActKeep, "the scheduled jobs could not all be read"
		}
		return
	}
	r.fresh = TakeSample(r.opt.ProcRoot, r.m, r.watched)
	for _, f := range todo {
		if u, ok := r.fresh.InUse(f.canon); ok {
			f.Action, f.Why = ActKeep, "in use now: "+useText(u)
			continue
		}
		if f.Rule == RuleModels {
			if u, ok := r.repoInUse(r.fresh, f.canon); ok {
				f.Action, f.Why = ActKeep, "in use now: "+useText(u)
				continue
			}
			for _, b := range f.linked {
				if u, ok := r.fresh.InUse(b); ok {
					f.Action, f.Why = ActKeep, "in use now: "+useText(u)
				}
			}
			if f.Action != ActDelete {
				continue
			}
		}
		before := r.free(f.Path)
		var err error
		switch f.Rule {
		case RuleWorktrees:
			err = r.removeWorktree(ctx, f)
		case RuleModels:
			err = r.removeModel(f)
		default:
			err = removeLocked(f.Path)
		}
		if after := r.free(f.Path); before >= 0 && after >= 0 {
			f.Freed = max(0, after-before)
		}
		if err != nil {
			f.Err = err.Error()
			if _, statErr := os.Lstat(f.Path); statErr == nil {
				continue // still there: a failure, reported
			}
		}
		f.Done = true
	}
}

// free is the free space of the disk a path is on, or -1.
func (r *runner) free(p string) int64 {
	var s syscall.Statfs_t
	for cur := p; ; cur = filepath.Dir(cur) {
		if err := syscall.Statfs(cur, &s); err == nil {
			return int64(s.Bavail) * int64(s.Bsize)
		}
		if cur == filepath.Dir(cur) {
			return -1
		}
	}
}

// errLocked means another process holds a lock on the directory: systemd-tmpfiles's convention for "do not age
// this tree".
var errLocked = errors.New("another process holds a lock on it")

// removeLocked deletes a directory or file while holding an exclusive BSD lock on it, which another holder of a
// lock refuses. A file is removed as it is.
func removeLocked(p string) error {
	st, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if st.IsDir() {
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			return errLocked
		}
	}
	return os.RemoveAll(p)
}

// useText is a use in words: what process used it and how.
func useText(u Use) string {
	how := map[string]string{"fd": "open in", "map": "mapped by", "cwd": "the working directory of", "exe": "the program of",
		"arg": "on the command line of"}[u.How]
	if how == "" {
		how = u.How + " by"
	}
	return fmt.Sprintf("%s %s (pid %d)", how, u.Comm, u.PID)
}

func itoa(n int) string { return strconv.Itoa(n) }

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// short is a path with the home directory as ~.
func short(p string) string {
	h := homeDir()
	if h != "/" && under(p, h) {
		return "~" + strings.TrimPrefix(p, h)
	}
	return p
}
