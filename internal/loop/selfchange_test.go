package loop

import (
	"reflect"
	"strings"
	"testing"

	"github.com/agent-sh/momentum/internal/store"
)

const (
	selfRules     = "package supervise\n\nimport \"fmt\"\n\nvar limit = 1\n\n// Limit is the limit.\nfunc Limit() int { return limit }\n\nfunc name() string { return fmt.Sprint(Limit()) }\n"
	selfRulesTest = "package supervise\n\nimport \"testing\"\n\nfunc TestLimitIsPositive(t *testing.T) {\n\tif Limit() <= 0 {\n\t\tt.Fatal(Limit())\n\t}\n}\n"
	selfNewTest   = "\n// The limit is two.\nfunc TestLimitIsTwo(t *testing.T) {\n\tif Limit() != 2 {\n\t\tt.Fatal(Limit())\n\t}\n}\n"
	rulesPath     = "internal/supervise/rules.go"
	rulesTestPath = "internal/supervise/zz_loop_test.go"
)

// The loop lands its own detectors, supervision and session readers, and nothing its judgment or authority rests on:
// the parts of those directories a replay over the store cannot read, the part that types into a session, and what
// the store builds on stay a person's, compared without case.
func TestTheLoopLandsAllButWhatItsJudgmentRestsOn(t *testing.T) {
	for _, c := range []string{"internal/detect/failures.go", "internal/detect/failures_test.go", "internal/detect/owner.go", "internal/detect/new.go", "internal/supervise/engine.go",
		"internal/supervise/rules_test.go", "internal/supervise/zz_loop_test.go", "internal/request/state.go",
		"sessions/claude/scan.go", "sessions/new/file.go", "internal/capacity/rules.go", "internal/capacity/rules_test.go",
		"README.md"} {
		if !selfLandable(c) || proposalProtected(c) {
			t.Errorf("%s: landable %v, protected %v", c, selfLandable(c), proposalProtected(c))
		}
	}
	for _, c := range []string{"internal/detect/sessions.go", "internal/detect/sessions_test.go", "internal/detect/detect.go", "internal/detect/keyed.go", "sessions/proc/x.go", "sessions/internal/text/x.go", "internal/supervise/replay.go", "internal/supervise/history.go",
		"sessions/act/act.go", "sessions/Act/x.go", "internal/detect/Sessions.go", "sessions/state/state.go", "sessions/sessions.go", "internal/evidenceview/view.go", "internal/source/transcripts.go", "internal/toolresult/x.go", "internal/plain/plain.go", "internal/capacity/names.go",
		"SPEC.md", "AGENTS.md", "LICENSE", "Internal/detect/failures.go"} {
		if selfLandable(c) || proposalProtected(c) {
			t.Errorf("%s is a person's to land: landable %v, protected %v", c, selfLandable(c), proposalProtected(c))
		}
	}
	for _, c := range []string{"internal/loop/gate.go", "internal/measure/rekey/rekey.go", "internal/measure/rekey/replay/main.go", "internal/sandbox/x.go",
		"internal/ro/ro.go", "internal/tracked/check/main.go", "internal/config/config.go", "internal/paths/paths.go", "internal/store/store.go",
		"internal/scorecard/score.go", "internal/weekly/week.go", "internal/capacity/acts.go", "internal/capacity/run.go", "cmd/momentum/loop.go",
		"cmd/momentum/supervise.go", "cmd/momentum/report.go", "cmd/momentum/new.go", "cmd/momentum/report_extra_test.go", "go.mod", "go.sum", "vendor/x/y.go", "Internal/Loop/gate.go"} {
		if selfLandable(c) || !proposalProtected(c) {
			t.Errorf("%s is what the loop's judgment or authority rests on: landable %v, protected %v", c, selfLandable(c), proposalProtected(c))
		}
	}
	if !isReplayed("internal/detect/failures.go") || isReplayed("internal/detect/sessions.go") || isReplayed("internal/supervise/rules.go") {
		t.Error("only a landable detector change is replayed")
	}
}

// A change the loop lands keeps its package within what it could do and keeps the checks its tests make; everything
// else in it is the review's.
func TestALandableFilesChangeKeepsItsPackageAndItsTests(t *testing.T) {
	two := strings.Replace(selfRules, "limit = 1", "limit = 2", 1)
	had := map[string]bool{"fmt": true, "testing": true}
	for name, c := range map[string]struct {
		file     string
		old, now string
		had      map[string]bool
		why      string
	}{
		"a rule's change":                  {rulesPath, selfRules, two, had, ""},
		"an initializer":                   {rulesPath, selfRules, two + "\nfunc init() { limit = 3 }\n", had, "initializer"},
		"a variable that runs code":        {rulesPath, selfRules, two + "\nvar started = fmt.Sprint(1)\n", had, "runs code"},
		"a value that runs code rewritten": {rulesPath, selfRules + "\nvar started = fmt.Sprint(1)\n", two + "\nvar started = fmt.Sprint(2)\n", had, "rewrites"},
		"a store write called":             {rulesPath, selfRules, two + "\nfunc closeAll(st interface{ CloseCase() }) { st.CloseCase() }\n", had, "store method"},
		"a store read called":              {rulesPath, selfRules, two + "\nfunc read(st interface{ Events() }) { st.Events() }\n", had, ""},
		"a constant":                       {rulesPath, selfRules, two + "\nconst grace = 2\n", had, ""},
		"a standard import":                {rulesPath, selfRules, strings.Replace(two, "import \"fmt\"", "import (\n\t\"fmt\"\n\t\"os\"\n)", 1) + "\nvar _ = os.Args\n", had, ""},
		"a module import":                  {rulesPath, selfRules, strings.Replace(two, "import \"fmt\"", "import (\n\t\"fmt\"\n\t\"github.com/agent-sh/momentum/internal/plain\"\n)", 1), had, ""},
		"a protected package's import":     {rulesPath, selfRules, strings.Replace(two, "import \"fmt\"", "import (\n\t\"fmt\"\n\t\"github.com/agent-sh/momentum/internal/config\"\n)", 1), had, "never changes"},
		"a risky import":                   {rulesPath, selfRules, strings.Replace(two, "import \"fmt\"", "import (\n\t\"fmt\"\n\t\"os/exec\"\n)", 1), had, "os/exec"},
		"the network":                      {rulesPath, selfRules, strings.Replace(two, "import \"fmt\"", "import (\n\t\"fmt\"\n\t\"net/http\"\n)", 1), had, "net/http"},
		"a risky import its package has":   {rulesPath, selfRules, strings.Replace(two, "import \"fmt\"", "import (\n\t\"fmt\"\n\t\"os/exec\"\n)", 1), map[string]bool{"os/exec": true}, ""},
		"another module":                   {rulesPath, selfRules, strings.Replace(two, "import \"fmt\"", "import \"golang.org/x/sys/unix\"", 1), had, "golang.org/x/sys/unix"},
		"a build constraint":               {rulesPath, selfRules, "//go:build never\n\n" + selfRules, had, "toolchain"},
		"a linked name":                    {rulesPath, selfRules, selfRules + "\n//go:linkname name runtime.x\n", had, "toolchain"},
		"a file that does not parse":       {rulesPath, selfRules, selfRules + "func {\n", had, "does not parse"},
		"a new file":                       {"internal/supervise/new.go", "", "package supervise\n\nfunc New() int { return 1 }\n", had, ""},
		"a file deleted":                   {rulesPath, selfRules, "", had, ""},
		"a test appended":                  {rulesTestPath, selfRulesTest, selfRulesTest + selfNewTest, had, ""},
		"a test's body rewritten":          {rulesTestPath, selfRulesTest, strings.Replace(selfRulesTest, "<= 0", "< 0", 1), had, ""},
		"a test taken away":                {rulesTestPath, selfRulesTest + selfNewTest, selfRulesTest, had, "TestLimitIsTwo"},
		"a test renamed":                   {rulesTestPath, selfRulesTest, strings.Replace(selfRulesTest, "TestLimitIsPositive", "TestLimitIsAbove", 1), had, "TestLimitIsPositive"},
		"a skip put inside an old test":    {rulesTestPath, selfRulesTest, strings.Replace(selfRulesTest, "\tif Limit", "\tt.Skip()\n\tif Limit", 1), had, "Skip"},
		"a TestMain":                       {rulesTestPath, selfRulesTest, selfRulesTest + "\nfunc TestMain(m *testing.M) {}\n", had, "TestMain"},
		"a test file deleted":              {rulesTestPath, selfRulesTest, "", had, "deletes a test file"},
		"a new test file":                  {"internal/supervise/new_test.go", "", selfRulesTest, had, ""},
		"a test's data":                    {"internal/detect/testdata/x.txt", "a", "b", had, ""},
		"a script":                         {"internal/detect/fix.sh", "a", "b", had, "no Go file"},
		"a test of one platform":           {"internal/supervise/x_arm64_test.go", "a", "b", had, "one platform"},
		"a file of one platform":           {"internal/supervise/x_linux.go", "a", "b", had, "one platform"},
		"an assembly file":                 {"internal/supervise/sys_amd64.s", "a", "b", had, "no Go file"},
	} {
		var old, now []byte
		if c.old != "" {
			old = []byte(c.old)
		}
		if c.now != "" {
			now = []byte(c.now)
		}
		if why := selfKept(c.file, old, now, c.had); c.why == "" && why != "" || c.why != "" && !strings.Contains(why, c.why) {
			t.Errorf("%s: why %q, want %q", name, why, c.why)
		}
	}
}

// The review of a change the loop lands itself is told what to rate high; a hand landing's and another
// repository's are not.
func TestOnlyALoopLandingsReviewIsToldItsTerms(t *testing.T) {
	c := store.Case{ID: "c1", Family: "failures"}
	own := reviewPrompt(c, Package{Repo: SelfRepo, Title: "t"})
	if !strings.Contains(own, selfReach) {
		t.Errorf("a loop landing's review: %s", own)
	}
	for name, p := range map[string]Package{"a hand landing": {Repo: SelfRepo, Hand: []string{"internal/x.go"}}, "another repository": {Repo: "agent-sh/other"}} {
		if strings.Contains(reviewPrompt(c, p), "lands this change itself") {
			t.Errorf("%s's review is told the loop lands it", name)
		}
	}
}

// A landable file reaches no further than its own code: it writes no variable of another package or of its package's
// frame, and changes no function its package variables' initializers run at every start.
func TestALandableFileWritesNoOtherPackagesState(t *testing.T) {
	frame := "package detect\n\nvar Versions = map[string]string{\"failures\": \"v2\"}\n\nvar Replayable = []string{\"failures\"}\n"
	init := "package detect\n\nimport \"strings\"\n\nvar shells = names()\n\nfunc names() []string { return strings.Fields(\"bash sh\") }\n\nfunc other() int { return 1 }\n"
	rules := "package detect\n\nvar ruleFuncs = map[string]func() int{\"a\": stored}\n\nfunc stored() int { return 1 }\n"
	facts := readFacts(map[string][]byte{"internal/detect/detect.go": []byte(frame), "internal/detect/failures.go": []byte(init), "internal/detect/rules.go": []byte(rules)})
	if _, ok := facts.initRun["stored"]; ok {
		t.Fatal("a function only stored in a map counts as run at the start")
	}
	if !facts.frame["Versions"] || !facts.frame["Replayable"] || facts.frame["shells"] {
		t.Fatalf("the frame: %v", facts.frame)
	}
	if _, ok := facts.initRun["names"]; !ok {
		t.Fatalf("what initializers run: %v", facts.initRun)
	}
	base := "package detect\n\nimport \"github.com/agent-sh/momentum/internal/paths\"\n\nfunc keyed() string { return paths.DB() }\n"
	for name, c := range map[string]struct{ file, old, now, why string }{
		"a body of its own":              {"internal/detect/owner.go", base, base + "\nfunc more() int { x := map[string]int{}; x[\"a\"] = 1; return x[\"a\"] }\n", ""},
		"another package's variable":     {"internal/detect/owner.go", base, base + "\nfunc set() { paths.Home = \"/x\" }\n", "another package"},
		"another package's map":          {"internal/detect/owner.go", base, base + "\nfunc set() { delete(paths.Roots, \"a\") }\n", "another package"},
		"its frame's map":                {"internal/detect/owner.go", base, base + "\nfunc set() { Versions[\"component-health\"] = \"v9\" }\n", "frame"},
		"its frame's slice":              {"internal/detect/owner.go", base, base + "\nfunc set() { Replayable = append(Replayable, \"sessions\") }\n", "frame"},
		"a function an initializer runs": {"internal/detect/failures.go", init, strings.Replace(init, "bash sh", "bash sh zsh", 1), "initializer runs"},
		"another function of its own":    {"internal/detect/failures.go", init, strings.Replace(init, "return 1", "return 2", 1), ""},
		"a local of its own":             {"internal/detect/owner.go", base, base + "\nfunc local() int { Versions := 1; return Versions }\n", ""},
		"its frame copied into":          {"internal/detect/owner.go", base, base + "\nfunc set() { copy(Replayable, []string{\"sessions\"}) }\n", "frame"},
	} {
		if why := pkgKept(c.file, []byte(c.old), []byte(c.now), facts); c.why == "" && why != "" || c.why != "" && !strings.Contains(why, c.why) {
			t.Errorf("%s: why %q, want %q", name, why, c.why)
		}
	}
	// the store's reads are its methods, by name: a name renamed away would let a write pass as one
	for _, r := range storeReads {
		_, onStore := reflect.TypeOf(&store.Store{}).MethodByName(r)
		_, onRun := reflect.TypeOf(&store.Run{}).MethodByName(r)
		if !onStore && !onRun {
			t.Errorf("%s is no method of the store's", r)
		}
	}
	for _, w := range []string{"CloseCase", "SweepCases", "Put", "SessionRun", "SaveStamps", "UpdateCase"} {
		if !storeWrite(w) {
			t.Errorf("%s may write and reads as a read", w)
		}
	}
}
