package loop

import (
	"os"
	"path"
	"path/filepath"
	"slices"
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

// A test file's change is the loop's to land only when it appends whole test functions: everything else that a
// change can do to a test file takes a check away, or runs before one, and is told from an append by parsing.
func TestOnlyAppendedTestFunctionsAreAnAddedCheck(t *testing.T) {
	if names, why := appendedTests([]byte(selfRulesTest), []byte(selfRulesTest+selfNewTest+"\nfunc Test_two(t *testing.T) {}\n")); why != "" ||
		!slices.Equal(names, []string{"TestLimitIsTwo", "Test_two"}) {
		t.Fatalf("two appended tests: %v %q", names, why)
	}
	for name, c := range map[string]struct{ old, changed, why string }{
		"a line of an old test rewritten": {selfRulesTest, strings.Replace(selfRulesTest, "Limit() <= 0", "Limit() < -5", 1) + selfNewTest, "changes or removes"},
		"a skip put inside an old test":   {selfRulesTest, strings.Replace(selfRulesTest, "\tif Limit", "\tt.Skip()\n\tif Limit", 1) + selfNewTest, "changes or removes"},
		"an old test removed":             {selfRulesTest, "package supervise\n\nimport \"testing\"\n" + selfNewTest, "changes or removes"},
		"a new file":                      {"", selfRulesTest, "new"},
		"nothing added":                   {selfRulesTest, selfRulesTest, "adds nothing"},
		"a comment alone":                 {selfRulesTest, selfRulesTest + "\n// a note\n", "no test function"},
		"a TestMain":                      {selfRulesTest, selfRulesTest + selfNewTest + "\nfunc TestMain(m *testing.M) {}\n", "no test"},
		"a TestMain that takes a T":       {selfRulesTest, selfRulesTest + "\nfunc TestMain(t *testing.T) {}\n", "no test"},
		"an initializer":                  {selfRulesTest, selfRulesTest + selfNewTest + "\nfunc init() { panic(1) }\n", "no test"},
		"a variable":                      {selfRulesTest, selfRulesTest + selfNewTest + "\nvar _ = func() int { panic(1) }()\n", "no test function"},
		"a type":                          {selfRulesTest, selfRulesTest + selfNewTest + "\ntype x int\n", "no test function"},
		"a helper function":               {selfRulesTest, selfRulesTest + selfNewTest + "\nfunc helper(t *testing.T) {}\n", "no test"},
		"a lower-case test name":          {selfRulesTest, selfRulesTest + "\nfunc Testing(t *testing.T) {}\n", "no test"},
		"a method":                        {selfRulesTest + "\ntype x int\n", selfRulesTest + "\ntype x int\n" + "\nfunc (x) TestX(t *testing.T) {}\n", "no test function"},
		"a benchmark's argument":          {selfRulesTest, selfRulesTest + "\nfunc TestB(b *testing.B) {}\n", "*testing.T"},
		"two arguments":                   {selfRulesTest, selfRulesTest + "\nfunc TestB(t, u *testing.T) {}\n", "*testing.T"},
		"a type parameter":                {selfRulesTest, selfRulesTest + "\nfunc TestB[T any](t *testing.T) {}\n", "no test function"},
		"a build constraint":              {selfRulesTest, selfRulesTest + "\n//go:build never\n" + selfNewTest, "toolchain"},
		"a linked name":                   {selfRulesTest, selfRulesTest + "\n//go:linkname TestL runtime.x\nfunc TestL(t *testing.T) {}\n", "toolchain"},
		"a block comment opened":          {selfRulesTest, selfRulesTest + "\n/*\n", "does not parse"},
		"a file that ended inside a line": {strings.TrimSuffix(selfRulesTest, "\n"), selfRulesTest + selfNewTest, "line's end"},
		"an import":                       {"package supervise\n\nimport \"testing\"\n", "package supervise\n\nimport \"testing\"\n\nimport \"os\"\n" + selfNewTest, "no test function"},
		"testing under another name":      {strings.Replace(selfRulesTest, "import \"testing\"", "import testing \"fmt\"", 1), strings.Replace(selfRulesTest, "import \"testing\"", "import testing \"fmt\"", 1) + selfNewTest, "import testing"},
		"an end inside a line":            {selfRulesTest, selfRulesTest + "\nfunc TestN(t *testing.T) {}", "line's end"},
		"an old file that does not parse": {"package supervise\n\nfunc {\n", "package supervise\n\nfunc {\n" + selfNewTest, "did not parse"},
	} {
		if names, why := appendedTests([]byte(c.old), []byte(c.changed)); why == "" || !strings.Contains(why, c.why) || names != nil {
			t.Errorf("%s: names %v, why %q, want a refusal with %q", name, names, why, c.why)
		}
	}
}

// The terms hold for the files they are for: the repository's own rules file changes nothing by standing as it is,
// and its test file takes an appended test.
func TestTheRepositorysOwnRulesFilesMeetTheTerms(t *testing.T) {
	rules, err := os.ReadFile("../../" + rulesPath)
	if err != nil {
		t.Fatal(err)
	}
	if why := rulesKept(rules, append(slices.Clone(rules), "\n// more is one more.\nfunc more(n int) int { return n + 1 }\n"...)); why != "" {
		t.Fatalf("the rules file with one more function: %q", why)
	}
	test, err := os.ReadFile("../../" + rulesTestPath)
	if err != nil {
		t.Fatal(err)
	}
	if names, why := appendedTests(test, append(slices.Clone(test), "\nfunc TestOneMore(t *testing.T) {\n\tif len(Rules) == 0 {\n\t\tt.Fatal(\"no rule\")\n\t}\n}\n"...)); why != "" || !slices.Equal(names, []string{"TestOneMore"}) {
		t.Fatalf("the rules' test file with one more test: %v %q", names, why)
	}
	// the loop's tests run after every other test of the package, as the package's test files stand
	for file, rules := range selfTests {
		names, err := filepath.Glob("../../" + path.Dir(file) + "/*_test.go")
		if err != nil || len(names) < 2 {
			t.Fatalf("the test files beside %s: %v %v", file, names, err)
		}
		files := map[string][]byte{}
		for _, name := range names {
			if files[filepath.Base(name)], err = os.ReadFile(name); err != nil {
				t.Fatal(err)
			}
		}
		if why := runLast(files, path.Base(file)); why != "" {
			t.Errorf("the tests of %s do not run last: %s", file, why)
		}
		if path.Dir(rules) != path.Dir(file) || !slices.Contains(selfChanges, rules) {
			t.Errorf("%s lands with %s, which is no rules file of its package", file, rules)
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

// A rules file's change is the loop's to land while it gives the file no other import, initializer or compiler
// directive, and the file was there.
func TestARulesFileKeepsItsImportsAndHasNoInitializer(t *testing.T) {
	if why := rulesKept([]byte(selfRules), []byte(strings.Replace(selfRules, "limit = 1", "limit = 2", 1)+"\n// Twice is the limit twice.\nfunc Twice() int { return 2 * Limit() }\n")); why != "" {
		t.Fatalf("a rule's change: %q", why)
	}
	for name, c := range map[string]struct{ changed, why string }{
		"another import":                        {strings.Replace(selfRules, "import \"fmt\"", "import (\n\t\"fmt\"\n\t\"os\"\n)", 1) + "\nvar _ = os.Args\n", "adds an import: os"},
		"an import under a name":                {strings.Replace(selfRules, "import \"fmt\"", "import fmt \"os\"", 1), "adds an import: os"},
		"an import for its effects":             {strings.Replace(selfRules, "import \"fmt\"", "import (\n\t\"fmt\"\n\t_ \"testing\"\n)", 1), "adds an import: testing"},
		"an initializer":                        {selfRules + "\nfunc init() { panic(1) }\n", "initializer"},
		"a build constraint":                    {"//go:build never\n\n" + selfRules, "toolchain"},
		"a linked name":                         {selfRules + "\n//go:linkname name runtime.x\n", "toolchain"},
		"another package":                       {strings.Replace(selfRules, "package supervise", "package other", 1), "renames"},
		"a file that does not parse":            {selfRules + "func {\n", "does not parse"},
		"a variable set by a function it calls": {selfRules + "\nvar _ = func() bool { limit = 9; return true }()\n", "whose value runs code"},
		"a variable set by a call":              {selfRules + "\nvar started = fmt.Sprint(1)\n", "whose value runs code"},
		"a variable that is a function":         {selfRules + "\nvar hook = func() {}\n", "whose value runs code"},
		"a function named as a builtin":         {selfRules + "\nfunc len(s string) int { return 0 }\n", "hides a predeclared one: len"},
		"a constant named true":                 {selfRules + "\nconst true = false\n", "hides a predeclared one: true"},
		"a type named string":                   {selfRules + "\ntype string int\n", "hides a predeclared one: string"},
	} {
		if why := rulesKept([]byte(selfRules), []byte(c.changed)); !strings.Contains(why, c.why) {
			t.Errorf("%s: why %q, want %q", name, why, c.why)
		}
	}
	if why := rulesKept(nil, []byte(selfRules)); !strings.Contains(why, "new") {
		t.Errorf("a new rules file: %q", why)
	}
	if why := rulesKept([]byte("//go:build linux\n\n"+selfRules), []byte("//go:build linux\n\n"+selfRules+"\nfunc more() {}\n")); why != "" {
		t.Errorf("a directive the file already held: %q", why)
	}
	// what the file held it may keep: only what the change brings is read against the terms
	held := selfRules + "\nvar started = fmt.Sprint(1)\n\nfunc init() { limit = 3 }\n"
	if why := rulesKept([]byte(held), []byte(held+"\nfunc more() { limit++ }\n")); why != "" {
		t.Errorf("a file that already held an initializer: %q", why)
	}
	if why := rulesKept([]byte(held), []byte(held+"\nfunc init() { limit = 4 }\n")); !strings.Contains(why, "initializer") {
		t.Errorf("a second initializer: %q", why)
	}
	// a constant runs nothing
	if why := rulesKept([]byte(selfRules), []byte(selfRules+"\nconst grace = float64(90) * 2\n")); why != "" {
		t.Errorf("a constant with a conversion: %q", why)
	}
}

// The loop's tests run after every other test of their package while the other test files leave the go command's
// order alone: none sorts after the loop's, is of the external test package, holds a TestMain, a fuzz test or an
// example, or runs a test in parallel.
func TestTheLoopsTestsRunLast(t *testing.T) {
	own := "package p\n\nimport \"testing\"\n\nfunc TestOwn(t *testing.T) { t.Parallel() }\n"
	other := "package p\n\nimport \"testing\"\n\nfunc TestOther(t *testing.T) {\n\tt.Run(\"sub\", func(t *testing.T) {})\n}\n\nfunc BenchmarkX(b *testing.B) {}\n"
	if why := runLast(map[string][]byte{"a_test.go": []byte(other), "zz_loop_test.go": []byte(own)}, "zz_loop_test.go"); why != "" {
		t.Fatalf("a package whose tests run in the files' order: %q", why)
	}
	for name, c := range map[string]struct{ file, body, why string }{
		"a later file":                    {"zzz_test.go", other, "sorts after"},
		"the external test package":       {"ext_test.go", strings.Replace(other, "package p", "package p_test", 1), "another package"},
		"a TestMain":                      {"a_test.go", other + "\nfunc TestMain(m *testing.M) {}\n", "TestMain"},
		"a fuzz test":                     {"a_test.go", other + "\nfunc FuzzX(f *testing.F) {}\n", "FuzzX"},
		"an example":                      {"a_test.go", other + "\nfunc ExampleX() {}\n", "ExampleX"},
		"a parallel test":                 {"a_test.go", other + "\nfunc TestP(t *testing.T) { t.Parallel() }\n", "parallel"},
		"a parallel subtest":              {"a_test.go", other + "\nfunc TestP(t *testing.T) {\n\tt.Run(\"s\", func(u *testing.T) { u.Parallel() })\n}\n", "parallel"},
		"Parallel as a method expression": {"a_test.go", other + "\nfunc TestP(t *testing.T) { (*testing.T).Parallel(t) }\n", "parallel"},
		"Parallel as a method value":      {"a_test.go", other + "\nfunc TestP(t *testing.T) {\n\tp := t.Parallel\n\tp()\n}\n", "parallel"},
		"a file that does not parse":      {"a_test.go", "package p\n\nfunc {\n", "does not parse"},
		"the loop's file not among it":    {"zz_loop_test.go", "", "not among"},
	} {
		files := map[string][]byte{"a_test.go": []byte(other), "zz_loop_test.go": []byte(own)}
		files[c.file] = []byte(c.body)
		if c.body == "" {
			delete(files, c.file)
		}
		if why := runLast(files, "zz_loop_test.go"); !strings.Contains(why, c.why) {
			t.Errorf("%s: %q, want %q", name, why, c.why)
		}
	}
}
