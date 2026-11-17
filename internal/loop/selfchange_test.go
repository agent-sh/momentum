package loop

import (
	"slices"
	"strings"
	"testing"
)

const (
	selfRules     = "package supervise\n\nimport \"fmt\"\n\nvar limit = 1\n\n// Limit is the limit.\nfunc Limit() int { return limit }\n\nfunc name() string { return fmt.Sprint(Limit()) }\n"
	selfRulesTest = "package supervise\n\nimport \"testing\"\n\nfunc TestLimitIsPositive(t *testing.T) {\n\tif Limit() <= 0 {\n\t\tt.Fatal(Limit())\n\t}\n}\n"
	selfNewTest   = "\n// The limit is two.\nfunc TestLimitIsTwo(t *testing.T) {\n\tif Limit() != 2 {\n\t\tt.Fatal(Limit())\n\t}\n}\n"
	rulesPath     = "internal/supervise/rules.go"
	rulesTestPath = "internal/supervise/rules_test.go"
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
		"an old file that does not parse": {"package supervise\n\nfunc {\n", "package supervise\n\nfunc {\n" + selfNewTest, "did not parse"},
	} {
		if names, why := appendedTests([]byte(c.old), []byte(c.changed)); why == "" || !strings.Contains(why, c.why) || names != nil {
			t.Errorf("%s: names %v, why %q, want a refusal with %q", name, names, why, c.why)
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
		"another import":             {strings.Replace(selfRules, "import \"fmt\"", "import (\n\t\"fmt\"\n\t\"os\"\n)", 1) + "\nvar _ = os.Args\n", "adds an import: os"},
		"an import under a name":     {strings.Replace(selfRules, "import \"fmt\"", "import fmt \"os\"", 1), "adds an import: os"},
		"an import for its effects":  {strings.Replace(selfRules, "import \"fmt\"", "import (\n\t\"fmt\"\n\t_ \"testing\"\n)", 1), "adds an import: testing"},
		"an initializer":             {selfRules + "\nfunc init() { panic(1) }\n", "initializer"},
		"a build constraint":         {"//go:build never\n\n" + selfRules, "toolchain"},
		"a linked name":              {selfRules + "\n//go:linkname name runtime.x\n", "toolchain"},
		"another package":            {strings.Replace(selfRules, "package supervise", "package other", 1), "renames"},
		"a file that does not parse": {selfRules + "func {\n", "does not parse"},
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
}
