package loop

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// reTestName is a test function's name as the go command reads one, the package's TestMain aside.
var reTestName = regexp.MustCompile(`^Test[^\p{Ll}]`)

// reDirective is a comment the toolchain reads: a //go: directive, a line directive, cgo's export, an old build tag.
var reDirective = regexp.MustCompile(`^(//go:|//line |/\*line |//export |//\s*\+build)`)

// appendedTests reads a test file's change in the loop's own repository: the names of the test functions it adds
// when it only appends whole test functions to the file, else why it does more. The loop adds checks and takes none
// away, and a count of lines cannot tell the two apart (a skip put inside an old test adds a line), so the change is
// parsed: what the file held is kept byte for byte, and every declaration after it is a plain
// `func TestXxx(t *testing.T)`. No TestMain, initializer, variable, type, import or compiler directive comes with
// them, so nothing the change adds runs before an old test or in its place. What an added test's body does is not
// read here: the file is the loop's own and its tests run last in its package (selfTests, runLast), so only tests
// the loop added run after it, and the review reads the body.
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
	case changed[len(changed)-1] != '\n':
		return nil, "it does not end with a line's end" // the next change could not append to it
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
// lands a rule's change, and these do more than change a rule. Another import. An initializer: an init function,
// or a package variable whose value holds a call or a function literal, which runs before every test and at the start of
// every process. A package-level name that hides a predeclared one (len, true, string), which changes every file of
// the package. A comment the toolchain reads (a build constraint, a linked name). Each is refused when the change
// brings it, not when the file held it. The file must parse before and after, and exist before: the loop adds no
// file to its own repository. These read the declarations the file holds. What a function's body does (sets
// another file's variable, reads the clock to tell today from a later day) is the review's to see, as it is for
// every rules change the loop has landed.
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
	if more := added(doings(fset, was, old), doings(fset, now, changed)); len(more) > 0 {
		return more[0]
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

// doings are what a rules file's declarations do beyond stating rules, each as a sentence: its init functions (by
// their count), the package variables whose value runs code (by their source), and the package-level names that
// hide predeclared ones.
func doings(fset *token.FileSet, f *ast.File, src []byte) []string {
	var out []string
	hides := func(name string) {
		if types.Universe.Lookup(name) != nil {
			out = append(out, "it declares a name that hides a predeclared one: "+name)
		}
	}
	inits := 0
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch spec := spec.(type) {
				case *ast.ValueSpec:
					for _, n := range spec.Names {
						hides(n.Name)
					}
					runs := false
					for _, v := range spec.Values {
						ast.Inspect(v, func(n ast.Node) bool {
							switch n.(type) {
							case *ast.CallExpr, *ast.FuncLit:
								runs = true
							}
							return !runs
						})
					}
					if runs && d.Tok == token.VAR { // a constant's value is computed when the package is compiled
						out = append(out, "it adds a package variable whose value runs code: "+string(src[fset.Position(spec.Pos()).Offset:fset.Position(spec.End()).Offset]))
					}
				case *ast.TypeSpec:
					hides(spec.Name.Name)
				}
			}
		case *ast.FuncDecl:
			if d.Recv != nil {
				continue
			}
			if hides(d.Name.Name); d.Name.Name == "init" {
				inits++
				out = append(out, "it adds an initializer (init function "+strconv.Itoa(inits)+")")
			}
		}
	}
	return out
}

// added are the entries of now that was does not hold.
func added(was, now []string) []string {
	var out []string
	for _, n := range now {
		if !slices.Contains(was, n) {
			out = append(out, n)
		}
	}
	return out
}

// runLast says why the tests of the file own would not run after every other test of its package, or "". files are
// the package's test files by name. The go command runs a package's tests file by file in the order of their
// names, and a file's in the order it holds them; these change that, and are refused in the other files: a file
// whose name sorts after own, a file of the package's external test package (run after the package's own), a
// TestMain (it decides what runs), a fuzz test or an example (run after the tests), and a test that calls Parallel
// (it waits for the others to end). A person's run with -shuffle or a count above one orders them otherwise; the
// host's does not.
func runLast(files map[string][]byte, own string) string {
	if files[own] == nil {
		return own + " is not among the package's test files"
	}
	fset := token.NewFileSet()
	pkg := ""
	for _, name := range slices.Sorted(maps.Keys(files)) {
		f, err := parser.ParseFile(fset, name, files[name], parser.SkipObjectResolution)
		if err != nil {
			return name + " does not parse"
		}
		if name == own {
			pkg = f.Name.Name
		}
	}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if name == own {
			continue
		}
		if name > own {
			return name + " sorts after " + own
		}
		f, _ := parser.ParseFile(fset, name, files[name], parser.SkipObjectResolution)
		if f.Name.Name != pkg {
			return name + " is in another package (" + f.Name.Name + "): its tests run after the package's own"
		}
		why := ""
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncDecl:
				if n.Recv == nil && (n.Name.Name == "TestMain" || strings.HasPrefix(n.Name.Name, "Fuzz") || strings.HasPrefix(n.Name.Name, "Example")) {
					why = name + " holds " + n.Name.Name + ", which does not run in the files' order"
				}
			case *ast.CallExpr:
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Parallel" && len(n.Args) == 0 {
					why = name + " holds a test that runs in parallel, after the others end"
				}
			}
			return why == ""
		})
		if why != "" {
			return why
		}
	}
	return ""
}
