package loop

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"slices"
	"strconv"
)

// reTestName is a test function's name as the go command reads one, the package's TestMain aside.
var reTestName = regexp.MustCompile(`^Test[^\p{Ll}]`)

// reDirective is a comment the toolchain reads: a //go: directive, a line directive, cgo's export, an old build tag.
var reDirective = regexp.MustCompile(`^(//go:|//line |/\*line |//export |// ?\+build)`)

// appendedTests reads a test file's change in the loop's own repository: the names of the test functions it adds
// when it only appends whole test functions to the file, else why it does more. The loop adds checks and takes none
// away, and a count of lines cannot tell the two apart (a skip put inside an old test adds a line), so the change is
// parsed: what the file held is kept byte for byte, and every declaration after it is a plain
// `func TestXxx(t *testing.T)`. No TestMain, initializer, variable, type, import or compiler directive comes with
// them, so nothing the change adds runs before an old test or in its place.
func appendedTests(old, changed []byte) (names []string, why string) {
	switch {
	case len(old) == 0:
		return nil, "the file is new"
	case !bytes.HasPrefix(changed, old):
		return nil, "it changes or removes what the file held"
	case len(changed) == len(old):
		return nil, "it adds nothing"
	case old[len(old)-1] != '\n':
		return nil, "the file did not end with a line's end"
	}
	fset := token.NewFileSet()
	was, err := parser.ParseFile(fset, "", old, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, "the file did not parse before the change"
	}
	now, err := parser.ParseFile(fset, "", changed, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, "it does not parse"
	}
	if !slices.ContainsFunc(was.Imports, func(i *ast.ImportSpec) bool { return i.Name == nil && i.Path.Value == `"testing"` }) {
		return nil, "the file does not import testing by its own name"
	}
	offset := func(p token.Pos) int { return fset.Position(p).Offset }
	if len(now.Decls) <= len(was.Decls) {
		return nil, "it adds no test function"
	}
	for i, d := range now.Decls {
		if i < len(was.Decls) {
			if offset(d.End()) > len(old) {
				return nil, "it goes on inside a declaration the file held"
			}
			continue
		}
		fn, ok := d.(*ast.FuncDecl)
		switch {
		case !ok:
			return nil, "it adds a declaration that is no test function"
		case offset(fn.Pos()) < len(old) || fn.Doc != nil && offset(fn.Doc.Pos()) < len(old):
			return nil, "it adds to a declaration the file held"
		case fn.Recv != nil || fn.Body == nil || fn.Type.TypeParams != nil || fn.Type.Results != nil:
			return nil, "it adds a function that is no test function: " + fn.Name.Name
		case !reTestName.MatchString(fn.Name.Name) || fn.Name.Name == "TestMain":
			return nil, "it adds a function the go command runs as no test: " + fn.Name.Name
		case !takesT(fn.Type.Params):
			return nil, "it adds a function that does not take a *testing.T: " + fn.Name.Name
		}
		names = append(names, fn.Name.Name)
	}
	for _, g := range now.Comments {
		for _, c := range g.List {
			if offset(c.Pos()) >= len(old) && reDirective.MatchString(c.Text) {
				return nil, "it adds a comment the toolchain reads"
			}
		}
	}
	return names, ""
}

// takesT reports a parameter list of one `*testing.T`.
func takesT(params *ast.FieldList) bool {
	if params == nil || len(params.List) != 1 || len(params.List[0].Names) > 1 {
		return false
	}
	star, ok := params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "testing" && sel.Sel.Name == "T"
}

// rulesKept says why a change to a rules file of the loop's own repository is a person's to land, or "": the loop
// lands a rule's change, and a change that gives the file another import or an initializer can do more than change
// a rule (end the process before a test runs, tell a test run from a real one). The file must parse before and
// after, and exist before: the loop adds no file to its own repository. A comment the toolchain reads (a build
// constraint, a linked name) is not added either.
func rulesKept(old, changed []byte) string {
	if len(old) == 0 {
		return "the file is new"
	}
	fset := token.NewFileSet()
	was, err := parser.ParseFile(fset, "", old, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return "the file did not parse before the change"
	}
	now, err := parser.ParseFile(fset, "", changed, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return "it does not parse"
	}
	if now.Name.Name != was.Name.Name {
		return "it renames the package"
	}
	had := map[string]bool{}
	for _, i := range was.Imports {
		had[importKey(i)] = true
	}
	for _, i := range now.Imports {
		if !had[importKey(i)] {
			path, _ := strconv.Unquote(i.Path.Value)
			return "it adds an import: " + path
		}
	}
	for _, d := range now.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "init" {
			return "it holds an initializer"
		}
	}
	read := map[string]bool{}
	for _, g := range was.Comments {
		for _, c := range g.List {
			read[c.Text] = reDirective.MatchString(c.Text)
		}
	}
	for _, g := range now.Comments {
		for _, c := range g.List {
			if reDirective.MatchString(c.Text) && !read[c.Text] {
				return "it adds a comment the toolchain reads"
			}
		}
	}
	return ""
}

func importKey(i *ast.ImportSpec) string {
	name := ""
	if i.Name != nil {
		name = i.Name.Name
	}
	return name + " " + i.Path.Value
}
