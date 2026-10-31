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
	root         string    // the directory it is deleted under (os.Root): no symlink leads the deletion out of it
	ino          inode     // its inode when the rule judged it: one put in its place since is not deleted
	parts        []string  // a target dir's own entries, when it holds others that Cargo does not build
	repo, branch string    // a worktree's common git directory and its branch
	head         string    // a worktree's commit when the rule judged it
	blobs        []string  // a model's shared-store blobs that go with it, canonical
	linked       []string  // every shared-store blob a model links, canonical
	class        string    // why a report-only finding is reported, as a word: its note's group
	none         bool      // a prune that removed nothing, as the tool lists it or uv counts it: no decided note
	unsaid       bool      // a prune whose tool did not say what it freed
	unsized      bool      // a prune whose size could not be read before it ran
	ownDirs      []ownDir  // the directories its deletion changed, each with its last change before (ownBefore)
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

// Writer is a process that wrote fast while a disk filled: at the look (Rate, over a few seconds), or since the run
// before (Written over Over), which shows a writer in bursts (a checkpoint save between two looks) and one whose
// children wrote and ended (a process takes in its reaped children's writes).
type Writer struct {
	PID     int     `json:"pid"`
	Start   int64   `json:"start,omitempty"` // its start, in clock ticks after boot: with the pid, which process
	Comm    string  `json:"comm"`
	Cwd     string  `json:"cwd"`
	Rate    float64 `json:"mib_per_s"`
	Written int64   `json:"written,omitempty"` // bytes it wrote, to any disk, over Over
	Over    float64 `json:"over_s,omitempty"`  // seconds: since the run before, or since it started after it
	OnDisk  bool    `json:"on_disk"`           // its working directory is on the disk
	Session string  `json:"session,omitempty"`
}

// since is a writer's MiB per second since the run before, or 0 when that is not known.
func (w Writer) since() float64 {
	if w.Over <= 0 {
		return 0
	}
	return float64(w.Written) / (1 << 20) / w.Over
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
	Warnings   []string      `json:"warnings,omitempty"`
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
	// beforeAct runs between the judgments and the deletions: a test's way into that gap.
	beforeAct func()
	// afterDelete runs after each deletion: a test's way to have the deletion's own change happen on its clock.
	afterDelete func(Finding)
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

	modelRoots    []root          // deepest first
	modelSet      map[string]bool // canonical
	stores        map[string]bool // canonical model roots with a Hugging Face shared blob store
	unknownStores map[string]bool // ones whose marker names a layout this build does not know
	keep          map[string]bool // canonical
	targetRoot    []root
	wtRoots       []root
	watchStart    map[string]time.Time // per canonical model root, the first run that watched it
	runTimes      []time.Time          // every run, oldest first, this one among them
	covered       []time.Duration      // the watched time up to each run (cover)
	blind         string               // why this run cannot trust its look at the processes, so deletes nothing
	dockerErr     string               // why Docker could not say which containers run: the run deletes nothing
	dockerAsked   bool                 // whether this run asked Docker (only when a rule scans)
	heads         *remoteHeads         // the remotes' branches, asked once per run
	changed       map[changeKey]bool   // whether a work tree changed in the last quietTime, per look
	own           map[string]ownChange // the directories this run's and recent runs' deletions changed
	writes        writesSnap           // each process's written bytes at this run
	prevWrites    writesSnap           // and at the last run before that could trust its look
	iv            map[int]wrote        // what each wrote in between
	res           *Result
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
	r := &runner{cfg: cfg, opt: opt, st: st, now: opt.Clock(), git: newGitInfo(), tools: opt.Tools, heads: newRemoteHeads()}
	if r.tools == nil {
		r.tools = execTools{uv: cfg.UV.Path, docker: cfg.Docker.Path}
	}
	r.m = ReadMounts(opt.MountInfo)
	r.ex = newExcluder(cfg.Exclude, r.m, r.git)
	r.res = &Result{At: r.now, DryRun: opt.DryRun}
	r.roots()

	clock := clockOf(opt.ProcRoot, opt.Clock()) // before the look lists /proc
	r.sample = TakeSample(opt.ProcRoot, r.m, r.watched)
	r.res.SampleTook, r.res.Procs, r.res.Unreadable = r.sample.Took, r.sample.Procs, r.sample.Unreadable
	r.res.SampleMS = r.sample.Took.Milliseconds()
	disks := r.diskSamples()
	if !opt.DryRun && st == nil {
		return nil, errors.New("a run that records needs the store")
	}
	var err error
	if r.seen, err = r.seenPaths(ctx); err != nil {
		return nil, err
	}
	if st != nil {
		if r.samplerStart, err = st.SamplerStart(ctx); err != nil {
			return nil, err
		}
	}
	if r.samplerStart.IsZero() {
		r.samplerStart = r.now // the first run: the record starts now
	}
	r.res.Since = r.samplerStart

	r.writes = readWrites(opt.ProcRoot, r.sample.Table, clock)
	if st != nil {
		snap, ws, err := st.Writes(ctx)
		if err != nil {
			return nil, err
		}
		r.prevWrites = writesSnap{snap: snap, ws: map[int]store.ProcWrite{}}
		for _, w := range ws {
			r.prevWrites.ws[w.PID] = w
		}
		carry(opt.ProcRoot, r.prevWrites, &r.writes)
	}
	fast := r.disk(ctx, disks)
	due := map[string]bool{}
	for _, rule := range Rules {
		if len(opt.Rules) > 0 && !contains(opt.Rules, rule) {
			continue
		}
		rr := RuleResult{Rule: rule, Live: cfg.IsLive(rule) && !opt.DryRun}
		switch ok, why, err := r.due(ctx, rule, fast); {
		case !cfg.configured(rule):
			rr.Why = "not configured"
		case err != nil:
			rr.Err = err.Error()
		case !ok:
			rr.Why = why
		default:
			due[rule] = true
			rr.Ran = true
		}
		r.res.Rules = append(r.res.Rules, rr)
	}
	if len(due) > 0 { // a container's bind mounts are its use: the sampler cannot read root's processes
		r.dockerAsked = true
		if err := r.addContainerUses(ctx, r.sample); err != nil {
			r.dockerErr = err.Error()
			r.res.Problems = append(r.res.Problems, "Docker could not be asked which containers run, so nothing is deleted this run: "+r.dockerErr)
		}
	}
	if err := r.trustSample(ctx); err != nil {
		return nil, err
	}
	seen := r.sampleSeen()
	if !opt.DryRun {
		samples := make([]store.DiskSample, 0, len(disks))
		for _, d := range disks {
			if d.Err == "" {
				samples = append(samples, store.DiskSample{Disk: d.Disk, At: r.now, Size: d.Size, Used: d.Used, Avail: d.Avail})
			}
		}
		if err := st.PutCapacityRun(ctx, store.CapacityRun{At: r.now, Sample: r.sample.Took, Procs: r.sample.Procs,
			Unreadable: r.sample.Unreadable, Blind: r.blind != ""}, samples, seen); err != nil {
			return nil, err
		}
		if r.blind == "" { // what the next run tells its writers by
			ws := make([]store.ProcWrite, 0, len(r.writes.ws))
			for _, w := range r.writes.ws {
				ws = append(ws, w)
			}
			if err := st.PutWrites(ctx, r.writes.snap, ws); err != nil {
				return nil, err
			}
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
	if err := r.watchRecord(ctx); err != nil {
		return nil, err
	}
	if len(due) > 0 {
		r.named, r.namedErr = LoadNamed(ctx, opt.Jobs, cfg.Names, r.m)
		if r.namedErr != nil {
			r.res.Problems = append(r.res.Problems, "the scheduled jobs could not all be read, so nothing is deleted this run: "+r.namedErr.Error())
		}
		if r.named != nil && len(r.named.Warnings) > 0 {
			r.res.Warnings = append(r.res.Warnings, fmt.Sprintf("%d files of system units this user cannot read, so what they name is not known: %s",
				len(r.named.Warnings), strings.Join(r.named.Warnings[:min(len(r.named.Warnings), 3)], "; ")))
		}
	}
	var found Found
	if due[RuleTargets] || due[RuleWorktrees] {
		found = Discover(append(shownOf(r.targetRoot), shownOf(r.wtRoots)...),
			func(p string) bool { return r.inTargetRoots(r.m.Canon(p)) }, func(p string) bool { return r.inWorktreeRoots(r.m.Canon(p)) }, nil)
	}
	// The rules that judge items run first, and their deletions follow at once, each checked again first; the
	// prunes come after, so a prune's minutes never stand between a judgment and its deletion.
	scan := func(rule string) {
		for i := range r.res.Rules {
			rr := &r.res.Rules[i]
			if rr.Rule != rule || !rr.Ran {
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
				rr.Findings, rr.Notes, err = r.uv(ctx, rr.Live && r.mayPrune(rr))
			case RuleDocker:
				rr.Findings, rr.Notes, err = r.docker(ctx, rr.Live && r.mayPrune(rr))
			}
			if err != nil {
				rr.Err = err.Error()
			}
			rr.Took = time.Since(start)
			rr.TookMS = rr.Took.Milliseconds()
		}
	}
	for _, rule := range []string{RuleModels, RuleTargets, RuleWorktrees} {
		scan(rule)
	}
	r.mountCheck()
	if err := r.act(ctx); err != nil {
		return r.res, err
	}
	for _, rule := range []string{RuleUV, RuleDocker} {
		scan(rule)
		if !opt.DryRun {
			if err := r.recordPrunes(ctx, rule); err != nil {
				return r.res, err
			}
		}
	}
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

// mayPrune reports whether a prune may run on this run: not when the scheduled jobs could not all be read, nor when
// the run cannot trust its look at the processes (the run says it deleted and pruned nothing), and it notes why in
// the rule's result.
func (r *runner) mayPrune(rr *RuleResult) bool {
	switch {
	case r.namedErr != nil:
		rr.Notes = append(rr.Notes, "no prune: the scheduled jobs could not all be read")
		return false
	case r.blind != "":
		rr.Notes = append(rr.Notes, "no prune: "+r.blind)
		return false
	}
	return true
}

// mountCheck reports, at judgment, every item a judging rule would delete that holds a mount point: a dry run lists
// it as act would leave it.
func (r *runner) mountCheck() {
	for i := range r.res.Rules {
		rr := &r.res.Rules[i]
		if rr.Rule != RuleModels && rr.Rule != RuleTargets && rr.Rule != RuleWorktrees {
			continue
		}
		for j := range rr.Findings {
			f := &rr.Findings[j]
			if f.Action != ActDelete {
				continue
			}
			if mp, ok := r.m.mountWithin(f.Path, f.canon); ok {
				f.Action, f.class, f.Why = ActReport, "mount", "it holds a mount point ("+mp+"): a deletion would reach past it"
			}
		}
	}
}

// watchRecord loads every run's time and when the sampler began to watch each model root, and marks the roots this
// run watches for the first time. A dry run adds itself as a run and marks nothing.
func (r *runner) watchRecord(ctx context.Context) error {
	r.watchStart = map[string]time.Time{}
	if r.st != nil {
		ts, err := r.st.RunTimes(ctx)
		if err != nil {
			return err
		}
		r.runTimes = ts
	}
	if r.opt.DryRun || len(r.runTimes) == 0 || r.runTimes[len(r.runTimes)-1].Before(r.now) {
		r.runTimes = append(r.runTimes, r.now)
	}
	r.cover()
	if !r.opt.DryRun { // a root no longer configured is watched no more: added back, it starts again
		marks, err := r.st.Marks(ctx, "watch:")
		if err != nil {
			return err
		}
		for m := range marks {
			if !r.modelSet[strings.TrimPrefix(m, "watch:")] {
				if err := r.st.ClearMark(ctx, m); err != nil {
					return err
				}
			}
		}
	}
	for _, x := range r.modelRoots {
		at, err := r.mark(ctx, "watch:"+x.canon)
		if err != nil {
			return err
		}
		if at.IsZero() {
			at = r.now
			if !r.opt.DryRun {
				if err := r.st.SetMark(ctx, "watch:"+x.canon, at); err != nil {
					return err
				}
			}
		}
		r.watchStart[x.canon] = at
	}
	return nil
}

// trustSample decides whether this run's look at the processes can be trusted to say what is in use. A process table
// of another PID namespace (a sandbox, a container) holds only its own processes, and a table far smaller than the
// recent runs' is not the machine's: either way every item would read as unused, so the run deletes nothing.
func (r *runner) trustSample(ctx context.Context) error {
	_, self := r.sample.Table[os.Getpid()]
	_, procfs := os.Lstat(filepath.Join(r.opt.ProcRoot, "self"))
	if l, err := os.Readlink(filepath.Join(r.opt.ProcRoot, "self", "ns", "pid")); err == nil && l != initPIDNS {
		r.blind = "it runs in a PID namespace of its own (" + l + "), where it cannot see the machine's processes"
	} else if r.sample.Procs == 0 {
		r.blind = "it read no process at all"
	} else if procfs == nil && !self {
		r.blind = "its own process is not among the processes it read: its process table is not the machine's"
	} else if r.st != nil {
		prior, err := r.st.RecentProcs(ctx, 24) // the runs before this one, which is not recorded yet
		if err != nil {
			return err
		}
		sort.Ints(prior)
		if len(prior) >= 12 {
			if med := prior[len(prior)/2]; r.sample.Procs < med/2 {
				r.blind = fmt.Sprintf("it read %d processes, against %d at the recent runs: its process table is not the machine's",
					r.sample.Procs, med)
			}
		}
	}
	if r.blind != "" {
		r.res.Problems = append(r.res.Problems, "nothing is deleted this run: "+r.blind)
	}
	return nil
}

// initPIDNS is the initial PID namespace, which the kernel numbers the same on every machine.
const initPIDNS = "pid:[4026531836]"

// roots makes the configured roots canonical.
func (r *runner) roots() {
	r.modelSet, r.stores, r.unknownStores, r.keep = map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, p := range r.cfg.Models.Roots {
		c := r.m.resolve(p)
		if !r.modelSet[c] {
			r.modelSet[c] = true
			r.modelRoots = append(r.modelRoots, root{shown: filepath.Clean(p), canon: c})
		}
		switch b, err := os.ReadFile(filepath.Join(p, "blobs", hfStoreMarker)); {
		case err == nil && strings.TrimSpace(string(b)) == "1":
			r.stores[c] = true
		case err == nil || !errors.Is(err, os.ErrNotExist):
			r.unknownStores[c] = true // a layout this build does not know, or a marker it cannot read
		}
	}
	sort.SliceStable(r.modelRoots, func(i, j int) bool { return len(r.modelRoots[i].canon) > len(r.modelRoots[j].canon) })
	for _, p := range r.cfg.Models.Keep { // as written and through its symlinks, as a job's path is
		r.keep[r.m.Canon(filepath.Clean(p))] = true
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
func (r *runner) due(ctx context.Context, rule string, fast bool) (bool, string, error) {
	if r.opt.DryRun || r.opt.Now || (fast && rule != RuleDocker) {
		return true, "", nil
	}
	last, err := r.mark(ctx, "scan:"+rule)
	if err != nil {
		return false, "", err
	}
	if every := r.cfg.every(rule); !last.IsZero() && r.now.Sub(last) < every {
		return false, fmt.Sprintf("scanned %s ago; scans every %s", r.now.Sub(last).Round(time.Minute), every), nil
	}
	return true, "", nil
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

// addContainerUses adds to a sample the host paths running Docker containers bind-mount: a server in a container
// runs as root, whose open files the sampler cannot read. Without Docker, or with a daemon that is not running, it
// adds nothing. A Docker that cannot be asked (its socket's permission, a timeout) is an error: a container may
// serve what the run would delete.
func (r *runner) addContainerUses(ctx context.Context, s *Sample) error {
	ids, err := r.tools.Docker(ctx, 15*time.Second, "ps", "-q")
	if errors.Is(err, errNoTool) || (err != nil && dockerDown(err)) {
		return nil
	}
	if err != nil {
		return err
	}
	if strings.TrimSpace(ids) == "" {
		return nil
	}
	out, err := r.tools.Docker(ctx, 30*time.Second, append([]string{"inspect", "--format",
		"{{.Name}}{{range .Mounts}}\t{{.Source}}{{end}}"}, strings.Fields(ids)...)...)
	if err != nil {
		return err
	}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(l), "\t")
		for _, src := range f[1:] {
			if !filepath.IsAbs(src) {
				continue
			}
			p := r.m.Canon(src)
			if !r.watched(p) && !r.holdsRoot(p) {
				continue
			}
			if _, ok := s.byPath[p]; !ok {
				u := Use{Path: p, How: "mount", Comm: "container " + strings.TrimPrefix(f[0], "/")}
				s.byPath[p] = u
				s.Uses = append(s.Uses, u)
			}
		}
	}
	return nil
}

// dockerDown reports a Docker daemon that is not running: then no container runs.
func dockerDown(err error) bool {
	m := err.Error()
	return strings.Contains(m, "Is the docker daemon running") || strings.Contains(m, "no such file or directory")
}

// holdsRoot reports whether a path holds one of the rules' roots: a mount of it gives a container every item.
func (r *runner) holdsRoot(p string) bool {
	for _, x := range append(append(append([]root{}, r.modelRoots...), r.targetRoot...), r.wtRoots...) {
		if under(x.canon, p) {
			return true
		}
	}
	return false
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
