package loop

import (
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"sort"

	"github.com/agent-sh/momentum/internal/store"
)

// Summary counts every investigation, including one the judge could not grade.
type Summary struct {
	Task, Family, Arm                                          string
	K, Ran, Reached, Partial, Missed, Wrong, CutShort, Skipped int
	Verdicts                                                   []string
	AnyReached, AllReached, Flipped                            bool
	USD, JudgeUSD, Minutes, TotalMinutes, Turns                float64
	Errors                                                     []string
}

func validVerdict(v string) bool {
	return v == "reached" || v == "partial" || v == "missed" || v == "wrong"
}

// Summarize uses the requested k even when a trial was skipped or failed.
func Summarize(trials []Trial, requested ...int) []Summary {
	by := map[string]*Summary{}
	var order []string
	for _, tr := range trials {
		key := tr.Task + "\x00" + tr.Arm
		s := by[key]
		if s == nil {
			s = &Summary{Task: tr.Task, Family: tr.Family, Arm: tr.Arm}
			by[key] = s
			order = append(order, key)
		}
		s.K++
		if tr.Skipped != "" {
			s.Skipped++
		} else if tr.Err != "" {
			s.Errors = append(s.Errors, tr.Err)
		} else if tr.Run.Status == store.RunFailed {
			s.Errors = append(s.Errors, tr.Run.Failure)
		}
		s.USD += tr.Run.USD
		s.JudgeUSD += tr.JudgeUSD
		if !tr.Run.Started.IsZero() {
			s.Ran++
			s.TotalMinutes += tr.Run.Ended.Sub(tr.Run.Started).Minutes()
			s.Turns += float64(tr.Run.Turns)
		}
		if tr.CutShort {
			s.CutShort++
		}
		if tr.Skipped != "" || !validVerdict(tr.Grade.Verdict) {
			continue
		}
		s.Verdicts = append(s.Verdicts, tr.Grade.Verdict)
		switch tr.Grade.Verdict {
		case "reached":
			s.Reached++
		case "partial":
			s.Partial++
		case "missed":
			s.Missed++
		case "wrong":
			s.Wrong++
		}
	}
	var out []Summary
	for _, key := range order {
		s := by[key]
		if len(requested) > 0 {
			s.K = requested[0]
		}
		if s.Ran > 0 {
			s.Minutes = s.TotalMinutes / float64(s.Ran)
			s.Turns /= float64(s.Ran)
		}
		s.AnyReached = s.Reached > 0
		s.AllReached = s.K > 0 && s.Reached == s.K && len(s.Verdicts) == s.K
		s.Flipped = s.Reached > 0 && s.Reached < len(s.Verdicts)
		out = append(out, *s)
	}
	return out
}

// ArmSummary reports the same denominators for each arm. An undefined rate is null in JSON.
type ArmSummary struct {
	Arm                                                                                                              string
	K, Tasks, Planned, Ran, Graded, Reached, Partial, Missed, Wrong, Errors, CutShort, Flips, Skipped, ComparedTasks int
	// Ungraded are the trials left out of the comparison because a judge could not grade them or their pair's other
	// arm; ComparedReached the root causes reached among the trials compared, which the rates per cause divide by.
	Ungraded, ComparedReached                                      int
	PassAtK, PassAllK                                              float64
	USD, JudgeUSD, Minutes                                         float64
	ComparedUSD, ComparedMinutes                                   float64
	USDPerTrial, MinutesPerTrial, USDPerReached, MinutesPerReached *float64
}
type TaskComparison struct {
	Task                       string
	LoopReached, NativeReached int
	MoreOften                  string
}
type Interval struct{ Low, High float64 }
type Comparison struct {
	Tasks []TaskComparison
	// Positive differences favor the loop. Resamples pair the arms by task.
	ReachedPerDollar, ReachedPerMinute *Interval
	Seed                               int64
	Resamples                          int
}
type EvaluationResults struct {
	Procedures map[string]Provenance `json:"procedures_by_family"`
	ID         string                `json:"id"`
	K          int                   `json:"k"`
	Provenance Provenance            `json:"provenance"`
	Trials     []Trial               `json:"trials"`
	Tasks      []Summary             `json:"tasks"`
	Arms       []ArmSummary          `json:"arms"`
	Comparison Comparison            `json:"comparison"`
}

func ratio(n float64, d int) *float64 {
	if d == 0 {
		return nil
	}
	x := n / float64(d)
	return &x
}

// Results compares both arms over the same k and retains the opaque-key to task mapping.
func (e *Eval) Results(trials []Trial) EvaluationResults {
	r := EvaluationResults{ID: e.ID, K: max(e.Trials, 1), Provenance: e.provenance(""), Trials: trials}
	r.Procedures = map[string]Provenance{}
	r.Provenance.Skills = map[string]string{}
	for _, tr := range trials {
		r.Procedures[tr.Family] = tr.Provenance
		for arm, hash := range tr.Provenance.Systems { // an investigation's system prompt is the same for every family
			r.Provenance.Systems[arm] = hash
		}
		for name, hash := range tr.Provenance.Skills {
			r.Provenance.Skills[name] = hash
		}
	}
	// Keep accounting for every run, but grade comparisons only where both arms ran the same trial.
	paired := len(e.Arms) > 1 || len(e.Arms) == 0 && !e.Native
	type pairKey struct {
		task string
		n    int
	}
	ran, graded := map[pairKey]map[string]bool{}, map[pairKey]map[string]bool{}
	for _, tr := range trials {
		k := pairKey{tr.Task, tr.N}
		if ran[k] == nil {
			ran[k], graded[k] = map[string]bool{}, map[string]bool{}
		}
		if !tr.Run.Started.IsZero() && tr.Skipped == "" {
			ran[k][tr.Arm] = true
			graded[k][tr.Arm] = validVerdict(tr.Grade.Verdict)
		}
	}
	accounted := append([]Trial(nil), trials...)
	var compared []Trial
	ungraded := map[string]int{}
	for i := range accounted {
		tr := &accounted[i]
		k := pairKey{tr.Task, tr.N}
		arms := ran[k]
		switch {
		case paired && (arms["loop"] != arms["native"]):
			tr.Skipped = "both arms did not run this trial"
		case tr.Skipped != "" || tr.Run.Started.IsZero():
		case !graded[k][tr.Arm] || paired && !(graded[k]["loop"] && graded[k]["native"]):
			// a trial without a grade says nothing of its investigation: it leaves the comparison with its pair
			ungraded[tr.Arm]++
		default:
			compared = append(compared, *tr)
		}
	}
	r.Trials = accounted
	r.Tasks = Summarize(accounted, r.K)
	by := map[string]*ArmSummary{}
	pairs := map[string]map[string]Summary{}
	for _, s := range r.Tasks {
		a := by[s.Arm]
		if a == nil {
			a = &ArmSummary{Arm: s.Arm, K: r.K}
			by[s.Arm] = a
		}
		a.Tasks++
		a.Planned += s.K
		a.Ran += s.Ran
		a.Graded += len(s.Verdicts)
		a.Reached += s.Reached
		a.Partial += s.Partial
		a.Missed += s.Missed
		a.Wrong += s.Wrong
		a.Errors += len(s.Errors)
		a.CutShort += s.CutShort
		a.Skipped += s.Skipped
		a.USD += s.USD
		a.JudgeUSD += s.JudgeUSD
		a.Minutes += s.TotalMinutes
	}
	for arm, n := range ungraded {
		if by[arm] != nil {
			by[arm].Ungraded = n
		}
	}
	for _, s := range Summarize(compared, r.K) {
		a := by[s.Arm]
		a.ComparedTasks++
		a.ComparedUSD += s.USD
		a.ComparedMinutes += s.TotalMinutes
		a.ComparedReached += s.Reached
		if s.AnyReached {
			a.PassAtK++
		}
		if s.AllReached {
			a.PassAllK++
		}
		if s.Flipped {
			a.Flips++
		}
		if pairs[s.Task] == nil {
			pairs[s.Task] = map[string]Summary{}
		}
		pairs[s.Task][s.Arm] = s
	}
	arms := e.Arms
	if len(arms) == 0 {
		arms = []string{"loop", "native"}
		if e.Native {
			arms = []string{"native"}
		}
	}
	for _, arm := range arms {
		if by[arm] == nil {
			by[arm] = &ArmSummary{Arm: arm, K: r.K}
		}
	}
	for _, arm := range []string{"loop", "native"} {
		if a := by[arm]; a != nil {
			if a.ComparedTasks > 0 {
				a.PassAtK /= float64(a.ComparedTasks)
				a.PassAllK /= float64(a.ComparedTasks)
			}
			a.USDPerTrial = ratio(a.USD, a.Ran)
			a.MinutesPerTrial = ratio(a.Minutes, a.Ran)
			a.USDPerReached = ratio(a.ComparedUSD, a.ComparedReached)
			a.MinutesPerReached = ratio(a.ComparedMinutes, a.ComparedReached)
			r.Arms = append(r.Arms, *a)
		}
	}
	r.Comparison = compareTasks(pairs)
	return r
}

// compareTasks bootstraps tasks, not individual trials, so repeated trials stay together.
func compareTasks(pairs map[string]map[string]Summary) Comparison {
	c := Comparison{Seed: 1, Resamples: 10000}
	var ids []string
	for id := range pairs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	complete := len(ids) > 0
	for _, id := range ids {
		l, lok := pairs[id]["loop"]
		n, nok := pairs[id]["native"]
		complete = complete && lok && nok
		more := "tie"
		if l.Reached > n.Reached {
			more = "loop"
		} else if n.Reached > l.Reached {
			more = "native"
		}
		if !lok || !nok {
			more = "not compared"
		}
		c.Tasks = append(c.Tasks, TaskComparison{id, l.Reached, n.Reached, more})
	}
	if !complete {
		return c
	}
	rng := rand.New(rand.NewSource(c.Seed))
	var dollars, minutes []float64
	dollarOK, minuteOK := true, true
	for range c.Resamples {
		var lr, nr, lu, nu, lm, nm float64
		for range ids {
			p := pairs[ids[rng.Intn(len(ids))]]
			l, n := p["loop"], p["native"]
			lr += float64(l.Reached)
			nr += float64(n.Reached)
			lu += l.USD
			nu += n.USD
			lm += l.TotalMinutes
			nm += n.TotalMinutes
		}
		if lu > 0 && nu > 0 {
			dollars = append(dollars, lr/lu-nr/nu)
		} else {
			dollarOK = false
		}
		if lm > 0 && nm > 0 {
			minutes = append(minutes, lr/lm-nr/nm)
		} else {
			minuteOK = false
		}
	}
	interval := func(xs []float64) *Interval { sort.Float64s(xs); return &Interval{xs[499], xs[9499]} }
	if dollarOK {
		c.ReachedPerDollar = interval(dollars)
	}
	if minuteOK {
		c.ReachedPerMinute = interval(minutes)
	}
	return c
}

// WriteResults also writes an empty evaluation and returns a failed write to the caller.
func (e *Eval) WriteResults(results EvaluationResults) error {
	if err := os.MkdirAll(e.Dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(e.Dir, "results.json"), raw, 0o600)
}
