package loop

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// reTestName is a test function's name as the go command reads one, the package's TestMain aside.
var reTestName = regexp.MustCompile(`^Test[^\p{Ll}]`)

// reDirective is a comment the toolchain reads: a //go: directive (an embed, a linked name, a generate line, a build
// constraint), a line directive, cgo's export, an old build tag.
var reDirective = regexp.MustCompile(`^(//go:|//line |/\*line |//export |//\s*\+build)`)

// riskyImports are the standard packages a landable file may import only where its package already does: they reach
// past the language's safety (unsafe, cgo's C), start other programs or load code (os/exec, plugin), make system calls
// directly (syscall), or open the network (net and its packages). A change the loop lands keeps each package inside
// what it could already do; a review reads what a body does with what it has.
var riskyImports = []string{"C", "unsafe", "os/exec", "plugin", "syscall", "net"}

// modulePath is the loop's own module: its packages are what a landable file may import besides the standard library.
const modulePath = "github.com/agent-sh/momentum"

// risky says whether an import path is one riskyImports names, or a package under one of them (net/http).
func risky(path string) bool {
	for _, r := range riskyImports {
		if path == r || strings.HasPrefix(path, r+"/") {
			return true
		}
	}
	return false
}

// std says whether an import path is the standard library's: its first element has no dot.
func std(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

// selfKept says why a change to a Go file of the loop's own repository that the loop may land by its path
// (selfLandable) is a person's all the same, or "". old is the file at the base (nil when the change adds it), now
// the file the change leaves (nil when it deletes it), and had the imports of the package's files at the base. The
// change keeps the package within what it could do and keeps the checks its tests make:
//   - it parses, and adds no comment the toolchain reads (a directive, a build constraint);
//   - every import it adds is of the loop's own module, or of the standard library but riskyImports, unless its
//     package already imports it;
//   - in a test file, it takes away no test function the file held (deleting the file takes them all), and adds no
//     TestMain (it decides what runs) and no call of Skip, SkipNow or Skipf (a test that skips proves nothing).
//
// What a function's body does (a test made to prove less, a rule that reads the clock) is the review's, which is
// told to rate high a change that makes a test prove less or widens what the loop may touch, land or spend.
func selfKept(name string, old, now []byte, had map[string]bool) string {
	test := strings.HasSuffix(name, "_test.go")
	if !strings.HasSuffix(name, ".go") {
		return ""
	}
	fset := token.NewFileSet()
	var was *ast.File
	if old != nil {
		f, err := parser.ParseFile(fset, "", old, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return "the file did not parse before the change"
		}
		was = f
	}
	if now == nil {
		if test && was != nil && len(testNames(was)) > 0 {
			return "it deletes a test file, and the tests it held"
		}
		return ""
	}
	is, err := parser.ParseFile(fset, "", now, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return "it does not parse"
	}
	before := map[string]bool{}
	read := map[string]bool{}
	if was != nil {
		for _, i := range was.Imports {
			path, _ := strconv.Unquote(i.Path.Value)
			before[path] = true
		}
		for _, g := range was.Comments {
			for _, c := range g.List {
				read[c.Text] = true
			}
		}
	}
	for _, i := range is.Imports {
		path, _ := strconv.Unquote(i.Path.Value)
		if before[path] || had[path] {
			continue
		}
		if !(path == modulePath || strings.HasPrefix(path, modulePath+"/") || std(path) && !risky(path)) {
			return "it adds an import its package does not have: " + path
		}
	}
	for _, g := range is.Comments {
		for _, c := range g.List {
			if reDirective.MatchString(c.Text) && !read[c.Text] {
				return "it adds a comment the toolchain reads"
			}
		}
	}
	if !test {
		return ""
	}
	if was != nil {
		kept := testNames(is)
		for _, n := range testNames(was) {
			if !slices.Contains(kept, n) {
				return "it takes away a test: " + n
			}
		}
	}
	if slices.Contains(testNames(is), "TestMain") && (was == nil || !slices.Contains(testNames(was), "TestMain")) {
		return "it adds a TestMain, which decides what the package's tests run"
	}
	if skips(is) > skipsOf(was) {
		return "it adds a call of Skip, SkipNow or Skipf: a test that skips proves nothing"
	}
	return ""
}

// testNames are the test functions a file declares, TestMain among them.
func testNames(f *ast.File) []string {
	var out []string
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && (reTestName.MatchString(fn.Name.Name) || fn.Name.Name == "TestMain") {
			out = append(out, fn.Name.Name)
		}
	}
	return out
}

// skips counts the calls of Skip, SkipNow and Skipf a file makes, through any receiver.
func skips(f *ast.File) int {
	n := 0
	ast.Inspect(f, func(x ast.Node) bool {
		if sel, ok := x.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Skip" || sel.Sel.Name == "SkipNow" || sel.Sel.Name == "Skipf") {
			n++
		}
		return true
	})
	return n
}

func skipsOf(f *ast.File) int {
	if f == nil {
		return 0
	}
	return skips(f)
}

// fileImports are the import paths of a Go file, or none when it does not parse.
func fileImports(src []byte) []string {
	f, err := parser.ParseFile(token.NewFileSet(), "", src, parser.ImportsOnly)
	if err != nil {
		return nil
	}
	var out []string
	for _, i := range f.Imports {
		path, _ := strconv.Unquote(i.Path.Value)
		out = append(out, path)
	}
	return out
}
