package loop

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"regexp"
	"slices"
	"strconv"
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
// them, so nothing the change adds runs before an old test or in its place. An added test sets no variable but its
// own (reaches): the tests after it, in this change's run and in every later one, see what they saw.
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
		if out := reaches(fn, declared(fn)); len(out) > 0 {
			return nil, "the test " + fn.Name.Name + " sets or takes the address of a variable that is not its own: " + out[0]
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
// lands a rule's change, and these do more than change a rule. Another import, or an initializer: an init function,
// or a package variable whose value calls something or is a function, which runs before every test and at the
// start of every process. A package-level name that hides a predeclared one (len, true, string), which changes
// every file of the package. A function that sets, or takes the address of, a variable the file does not declare:
// another file's, or another package's. A comment the toolchain reads (a build constraint, a linked name). Each is
// refused when the change brings it, not when the file held it. The file must parse before and after, and exist
// before: the loop adds no file to its own repository. What a rule's own code does with what it has (a rule that
// reads the clock to tell today from a later day) is the review's to see.
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

// doings are what a rules file does beyond its rules, each as a sentence: the package variables whose value runs
// code (by their source), the package-level names that hide predeclared ones, and the variables its functions set
// or take the address of without declaring them.
func doings(fset *token.FileSet, f *ast.File, src []byte) []string {
	var out []string
	own := declared(f)
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch spec := spec.(type) {
				case *ast.ValueSpec:
					for _, n := range spec.Names {
						if types.Universe.Lookup(n.Name) != nil {
							out = append(out, "it declares a name that hides a predeclared one: "+n.Name)
						}
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
					if runs {
						out = append(out, "it adds a package variable whose value runs code: "+string(src[fset.Position(spec.Pos()).Offset:fset.Position(spec.End()).Offset]))
					}
				case *ast.TypeSpec:
					if types.Universe.Lookup(spec.Name.Name) != nil {
						out = append(out, "it declares a name that hides a predeclared one: "+spec.Name.Name)
					}
				}
			}
		case *ast.FuncDecl:
			if d.Recv == nil && types.Universe.Lookup(d.Name.Name) != nil {
				out = append(out, "it declares a name that hides a predeclared one: "+d.Name.Name)
			}
			for _, name := range reaches(d, own) {
				out = append(out, "it sets or takes the address of a variable the file does not declare: "+name)
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

// declared are the names a node declares: variables, constants, parameters, results, receivers, types and
// functions, wherever they stand in it.
func declared(node ast.Node) map[string]bool {
	names := map[string]bool{}
	ast.Inspect(node, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if n.Tok == token.DEFINE {
				for _, l := range n.Lhs {
					if id, ok := l.(*ast.Ident); ok {
						names[id.Name] = true
					}
				}
			}
		case *ast.RangeStmt:
			if n.Tok == token.DEFINE {
				for _, e := range []ast.Expr{n.Key, n.Value} {
					if id, ok := e.(*ast.Ident); ok {
						names[id.Name] = true
					}
				}
			}
		case *ast.ValueSpec:
			for _, id := range n.Names {
				names[id.Name] = true
			}
		case *ast.TypeSpec:
			names[n.Name.Name] = true
		case *ast.Field:
			for _, id := range n.Names {
				names[id.Name] = true
			}
		case *ast.FuncDecl:
			if n.Recv == nil {
				names[n.Name.Name] = true
			}
		}
		return true
	})
	return names
}

// reaches are the variables a function sets, or takes the address of, that are not among own: by the name its
// expression starts from, so `limit = 2`, `table[k] = v`, `other.Field++` and `&limit` each name theirs.
func reaches(fn *ast.FuncDecl, own map[string]bool) []string {
	var out []string
	root := func(e ast.Expr) {
		for {
			switch x := e.(type) {
			case *ast.ParenExpr:
				e = x.X
			case *ast.IndexExpr:
				e = x.X
			case *ast.IndexListExpr:
				e = x.X
			case *ast.SelectorExpr:
				e = x.X
			case *ast.StarExpr:
				e = x.X
			case *ast.SliceExpr:
				e = x.X
			case *ast.Ident:
				if x.Name != "_" && !own[x.Name] && !slices.Contains(out, x.Name) {
					out = append(out, x.Name)
				}
				return
			default:
				return // a call's result, a literal: nothing named is set
			}
		}
	}
	ast.Inspect(fn, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if n.Tok != token.DEFINE {
				for _, l := range n.Lhs {
					root(l)
				}
			}
		case *ast.IncDecStmt:
			root(n.X)
		case *ast.RangeStmt:
			if n.Tok == token.ASSIGN {
				for _, e := range []ast.Expr{n.Key, n.Value} {
					if e != nil {
						root(e)
					}
				}
			}
		case *ast.UnaryExpr:
			if n.Op == token.AND {
				if _, lit := n.X.(*ast.CompositeLit); !lit {
					root(n.X)
				}
			}
		}
		return true
	})
	return out
}
