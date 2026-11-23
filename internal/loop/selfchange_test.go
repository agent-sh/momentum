package loop

import (
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

// The loop lands its own detectors, supervision, session readers and printing commands, and nothing its judgment or
// authority rests on: the parts of those directories a replay over the store cannot read, and the part that types
// into a session, stay a person's.
func TestTheLoopLandsAllButWhatItsJudgmentRestsOn(t *testing.T) {
	for _, c := range []string{"internal/detect/failures.go", "internal/detect/failures_test.go", "internal/detect/new.go", "internal/supervise/engine.go",
		"internal/supervise/rules_test.go", "internal/supervise/zz_loop_test.go", "internal/evidenceview/view.go", "internal/request/state.go",
		"sessions/claude/scan.go", "sessions/new/file.go", "internal/capacity/rules.go", "internal/capacity/rules_test.go",
		"README.md"} {
		if !selfLandable(c) || proposalProtected(c) {
			t.Errorf("%s: landable %v, protected %v", c, selfLandable(c), proposalProtected(c))
		}
	}
	for _, c := range []string{"internal/detect/sessions.go", "internal/detect/sessions_test.go", "internal/supervise/replay.go", "internal/supervise/history.go",
		"sessions/act/act.go", "internal/source/transcripts.go", "internal/toolresult/x.go", "internal/plain/plain.go", "internal/capacity/names.go",
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
		"a rule's change":                {rulesPath, selfRules, two, had, ""},
		"an initializer":                 {rulesPath, selfRules, two + "\nfunc init() { limit = 3 }\n", had, ""},
		"a standard import":              {rulesPath, selfRules, strings.Replace(two, "import \"fmt\"", "import (\n\t\"fmt\"\n\t\"os\"\n)", 1) + "\nvar _ = os.Args\n", had, ""},
		"a module import":                {rulesPath, selfRules, strings.Replace(two, "import \"fmt\"", "import (\n\t\"fmt\"\n\t\"github.com/agent-sh/momentum/internal/plain\"\n)", 1), had, ""},
		"a protected package's import":   {rulesPath, selfRules, strings.Replace(two, "import \"fmt\"", "import (\n\t\"fmt\"\n\t\"github.com/agent-sh/momentum/internal/config\"\n)", 1), had, "never changes"},
		"a risky import":                 {rulesPath, selfRules, strings.Replace(two, "import \"fmt\"", "import (\n\t\"fmt\"\n\t\"os/exec\"\n)", 1), had, "os/exec"},
		"the network":                    {rulesPath, selfRules, strings.Replace(two, "import \"fmt\"", "import (\n\t\"fmt\"\n\t\"net/http\"\n)", 1), had, "net/http"},
		"a risky import its package has": {rulesPath, selfRules, strings.Replace(two, "import \"fmt\"", "import (\n\t\"fmt\"\n\t\"os/exec\"\n)", 1), map[string]bool{"os/exec": true}, ""},
		"another module":                 {rulesPath, selfRules, strings.Replace(two, "import \"fmt\"", "import \"golang.org/x/sys/unix\"", 1), had, "golang.org/x/sys/unix"},
		"a build constraint":             {rulesPath, selfRules, "//go:build never\n\n" + selfRules, had, "toolchain"},
		"a linked name":                  {rulesPath, selfRules, selfRules + "\n//go:linkname name runtime.x\n", had, "toolchain"},
		"a file that does not parse":     {rulesPath, selfRules, selfRules + "func {\n", had, "does not parse"},
		"a new file":                     {"internal/supervise/new.go", "", "package supervise\n\nfunc New() int { return 1 }\n", had, ""},
		"a file deleted":                 {rulesPath, selfRules, "", had, ""},
		"a test appended":                {rulesTestPath, selfRulesTest, selfRulesTest + selfNewTest, had, ""},
		"a test's body rewritten":        {rulesTestPath, selfRulesTest, strings.Replace(selfRulesTest, "<= 0", "< 0", 1), had, ""},
		"a test taken away":              {rulesTestPath, selfRulesTest + selfNewTest, selfRulesTest, had, "TestLimitIsTwo"},
		"a test renamed":                 {rulesTestPath, selfRulesTest, strings.Replace(selfRulesTest, "TestLimitIsPositive", "TestLimitIsAbove", 1), had, "TestLimitIsPositive"},
		"a skip put inside an old test":  {rulesTestPath, selfRulesTest, strings.Replace(selfRulesTest, "\tif Limit", "\tt.Skip()\n\tif Limit", 1), had, "Skip"},
		"a TestMain":                     {rulesTestPath, selfRulesTest, selfRulesTest + "\nfunc TestMain(m *testing.M) {}\n", had, "TestMain"},
		"a test file deleted":            {rulesTestPath, selfRulesTest, "", had, "deletes a test file"},
		"a new test file":                {"internal/supervise/new_test.go", "", selfRulesTest, had, ""},
		"a file that is not Go":          {"internal/detect/testdata/x.txt", "a", "b", had, ""},
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
