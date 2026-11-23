package loop

import (
	"reflect"

	"github.com/agent-sh/momentum/internal/store"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path"
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
// past the language's safety (unsafe, cgo's C), start other programs or load code (os/exec, plugin), reach any value by its name (reflect), make system calls
// directly (syscall), or open the network (net and its packages). A change the loop lands keeps each package inside
// what it could already do; a review reads what a body does with what it has.
var riskyImports = []string{"C", "unsafe", "os/exec", "plugin", "syscall", "net", "reflect"}

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
//   - it is a Go file that builds on every platform (no _GOOS or _GOARCH in its name), a document or a test's data
//     under testdata/: an assembly, C or object file, and any other kind of file, reaches past the language;
//   - it parses, and adds no comment the toolchain reads (a directive, a build constraint);
//   - outside a test file, it adds no initializer (an init function, or a package variable whose value holds a call or
//     a function literal), which runs in every process that imports the package, the protected ones among them, and
//     no package-level name that hides a predeclared one;
//   - every import it adds is of the loop's own module but the packages no change touches (ProposalProtected), or of
//     the standard library but riskyImports, unless its package already imports it;
//   - in a test file, it takes away no test function the file held (deleting the file takes them all), and adds no
//     TestMain (it decides what runs) and no call of Skip, SkipNow or Skipf (a test that skips proves nothing).
//
// What a function's body does (a test made to prove less, a rule that reads the clock) is the review's, which is
// told to rate high a change that makes a test prove less or widens what the loop may touch, land or spend.
func selfKept(name string, old, now []byte, had map[string]bool, calls ...map[string]bool) string {
	test := strings.HasSuffix(name, "_test.go")
	if why := fileKind(name); why != "" {
		return why
	}
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
		own := path == modulePath || strings.HasPrefix(path, modulePath+"/")
		if own && proposalProtected(strings.TrimPrefix(path, modulePath+"/")+"/") {
			// what the loop's judgment rests on, which a package that did not import it could reach to change
			return "it adds an import of a package the loop never changes: " + path
		}
		if !(own || std(path) && !risky(path)) {
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
		if more := added(doings(is, now), doings(was, old)); len(more) > 0 {
			return more[0]
		}
		called := map[string]bool{}
		for _, c := range calls {
			for k := range c {
				called[k] = true
			}
		}
		for _, m := range selectors(is) {
			if storeWrite(m) && !called[m] && !slices.Contains(selectors(was), m) {
				return "it calls a store method its package's code does not call, which may write what the loop's judgment rests on: " + m
			}
		}
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

// knownOS and knownArch are the GOOS and GOARCH values a file's name can restrict it to (go/build's lists).
var knownOS = strings.Fields("aix android darwin dragonfly freebsd hurd illumos ios js linux nacl netbsd openbsd plan9 solaris wasip1 windows zos")
var knownArch = strings.Fields("386 amd64 amd64p32 arm armbe arm64 arm64be loong64 mips mipsle mips64 mips64le mips64p32 mips64p32le ppc ppc64 ppc64le riscv riscv64 s390 s390x sparc sparc64 wasm")

// fileKind says why a file of a landable directory is no kind the loop lands, or "".
func fileKind(name string) string {
	base := path.Base(name)
	switch {
	case strings.Contains("/"+path.Dir(name)+"/", "/testdata/"):
		return "" // a test's data, read by the test, never built
	case strings.HasSuffix(base, ".md"):
		return ""
	case !strings.HasSuffix(base, ".go"):
		return "it is no Go file: an assembly, C, object or other file can reach past the language"
	}
	stem := strings.TrimSuffix(strings.TrimSuffix(base, ".go"), "_test")
	parts := strings.Split(stem, "_")
	for _, p := range parts[1:] {
		if slices.Contains(knownOS, p) || slices.Contains(knownArch, p) {
			return "its name builds it on one platform only, which the check's platform may not run"
		}
	}
	return ""
}

// doings are what a file's declarations do beyond declaring: its init functions, the package variables whose value
// runs code, and the package-level names that hide predeclared ones, each as a sentence.
func doings(f *ast.File, src []byte) []string {
	if f == nil {
		return nil
	}
	fset := token.NewFileSet()
	f2, err := parser.ParseFile(fset, "", src, parser.SkipObjectResolution)
	if err == nil {
		f = f2 // positions of this file set, to read each value's source
	}
	var out []string
	inits := 0
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
					if runs && d.Tok == token.VAR {
						// its source with it: a value that runs code, rewritten, is another initializer
						text := strings.Join(names(spec.Names), ", ")
						if err == nil {
							text += " = " + string(src[fset.Position(spec.Pos()).Offset:fset.Position(spec.End()).Offset])
						}
						out = append(out, "it adds or rewrites a package variable whose value runs code: "+text)
					}
				case *ast.TypeSpec:
					if types.Universe.Lookup(spec.Name.Name) != nil {
						out = append(out, "it declares a name that hides a predeclared one: "+spec.Name.Name)
					}
				}
			}
		case *ast.FuncDecl:
			if d.Recv != nil {
				continue
			}
			if types.Universe.Lookup(d.Name.Name) != nil {
				out = append(out, "it declares a name that hides a predeclared one: "+d.Name.Name)
			}
			if d.Name.Name == "init" {
				inits++
				out = append(out, "it adds an initializer (init function "+strconv.Itoa(inits)+")")
			}
		}
	}
	return out
}

func names(ids []*ast.Ident) []string {
	var out []string
	for _, i := range ids {
		out = append(out, i.Name)
	}
	return out
}

// added are the entries of now that was does not hold.
func added(now, was []string) []string {
	var out []string
	for _, n := range now {
		if !slices.Contains(was, n) {
			out = append(out, n)
		}
	}
	return out
}

// selectors are the names a file selects (x.Name), each once.
func selectors(f *ast.File) []string {
	if f == nil {
		return nil
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && !slices.Contains(out, sel.Sel.Name) {
			out = append(out, sel.Sel.Name)
		}
		return true
	})
	return out
}

// storeReads are the store's methods that only read, by name: a method of *store.Store or *store.Run not listed here,
// a new one too, may write. Close only ends the handle.
var storeReads = strings.Fields(`Activity AgentEventBetween AgentParent APIErrors AskAnswered Before CallsOf CaseByID CaseEvidence
	CaseLive CaseLog CaseLogBetween CaseLogCount CaseLogLimit Cases Children Close Counts Cursor DiskSamples EarlierResults EventByID
	EventByKey Events EvidenceEvents FileCursors FileSeen FirstEvent FirstHarnessEvent FirstOf FirstPartyCalls Fixable FixedStop
	GuardRecurred HandFixes HarnessReadPast HarnessReadThrough HarnessReadWindowThrough HasSession Incomplete KnownLoopRunDirs
	LastActionAt LoopRunByID LoopRuns LoopSpend LoopSpendMicros LoopTurn Marks Notes Origins OwnerSessions OwnerTurns
	OwnerTurnsBetween PartialReadAfter Path PhasesSince QuietCases ReadThrough ReadWindowThrough RecentProcs ResumedAt RunTimes
	SamplerStart ScorecardValue ScorecardWeeks SeenPaths SentTexts Seq SessionStamps Skipped Sources Stamps Stored SuperviseEvents
	TextsHolding ThreadEvents ThreadFirst ThreadParentMeta ThreadsByPrefix ThreadSpans ThreadsSince Timeline ToolCallsLike Trackers
	UnreadEvidence UnreadSources Writes`)

// storeWrite says whether a name is a method of the store (*store.Store, or a sessions run's *store.Run) that may
// write: any but storeReads.
func storeWrite(name string) bool {
	_, onStore := reflect.TypeOf(&store.Store{}).MethodByName(name)
	_, onRun := reflect.TypeOf(&store.Run{}).MethodByName(name)
	return (onStore || onRun) && !slices.Contains(storeReads, name)
}

// pkgFacts are what a package holds at the base that a change to one of its landable files is read against: the
// package-level variables its files a person lands declare (frame), and the functions its package variables'
// initializers run, by their source (initRun), within the package, by plain name.
type pkgFacts struct {
	frame   map[string]bool
	initRun map[string]string
}

// readFacts reads a package's facts from its Go files at the base, each by its name and its bytes.
func readFacts(files map[string][]byte) pkgFacts {
	f := pkgFacts{frame: map[string]bool{}, initRun: map[string]string{}}
	funcs := map[string]*ast.FuncDecl{}
	srcs := map[string][]byte{}
	fsets := map[string]*token.FileSet{}
	var roots []string
	for name, b := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "", b, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		for _, d := range file.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil {
					funcs[d.Name.Name], srcs[d.Name.Name], fsets[d.Name.Name] = d, b, fset
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					v, ok := spec.(*ast.ValueSpec)
					if !ok || d.Tok != token.VAR {
						continue
					}
					if !selfLandable(name) {
						for _, n := range v.Names {
							f.frame[n.Name] = true
						}
					}
					for _, val := range v.Values {
						roots = append(roots, calledNames(val)...)
					}
				}
			}
		}
	}
	for len(roots) > 0 {
		n := roots[0]
		roots = roots[1:]
		d, ok := funcs[n]
		if _, seen := f.initRun[n]; !ok || seen {
			continue
		}
		f.initRun[n] = funcSource(fsets[n], srcs[n], d)
		roots = append(roots, calledNames(d.Body)...)
	}
	return f
}

// calledNames are the plain names a node calls or refers to as a value (a function passed on is run later).
func calledNames(n ast.Node) []string {
	var out []string
	if n == nil {
		return nil
	}
	ast.Inspect(n, func(x ast.Node) bool {
		if id, ok := x.(*ast.Ident); ok {
			out = append(out, id.Name)
		}
		return true
	})
	return out
}

func funcSource(fset *token.FileSet, src []byte, d *ast.FuncDecl) string {
	return string(src[fset.Position(d.Pos()).Offset:fset.Position(d.End()).Offset])
}

// pkgKept says why a change to a landable Go file reaches past its own code, against its package's facts, or "": it
// assigns to, or takes the address of, a variable of another package or of its package's frame (a file a person
// lands), or it changes or adds a function its package variables' initializers run, which runs at the start of every
// process that imports the package, the protected ones among them. What a body reaches otherwise (through a method,
// reflect, a pointer it was given) is the review's.
func pkgKept(name string, old, now []byte, facts pkgFacts) string {
	if now == nil || !strings.HasSuffix(name, ".go") {
		return ""
	}
	fset := token.NewFileSet()
	is, err := parser.ParseFile(fset, "", now, parser.SkipObjectResolution)
	if err != nil {
		return ""
	}
	var was *ast.File
	ofset := token.NewFileSet()
	if old != nil {
		was, _ = parser.ParseFile(ofset, "", old, parser.SkipObjectResolution)
	}
	if more := added(writes(is), writes(was)); len(more) > 0 {
		return "it writes a variable of another package or of its package's frame: " + more[0]
	}
	for _, more := range added(writesOf(is, facts.frame), writesOf(was, facts.frame)) {
		return "it writes a variable of its package's frame, which a person lands: " + more
	}
	if strings.HasSuffix(name, "_test.go") {
		return ""
	}
	before := map[string]string{}
	if was != nil {
		for _, d := range was.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil {
				before[fn.Name.Name] = funcSource(ofset, old, fn)
			}
		}
	}
	for _, d := range is.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv != nil {
			continue
		}
		if _, runs := facts.initRun[fn.Name.Name]; runs && funcSource(fset, now, fn) != before[fn.Name.Name] {
			return "it changes a function a package variable's initializer runs at every start: " + fn.Name.Name
		}
	}
	return ""
}

// writes are the targets a file assigns to, increments, deletes from or takes the address of that belong to another
// package (pkg.Name, pkg.Name[k], pkg.Name.f), as text.
func writes(f *ast.File) []string {
	if f == nil {
		return nil
	}
	imported := map[string]bool{}
	for _, i := range f.Imports {
		p, _ := strconv.Unquote(i.Path.Value)
		n := path.Base(p)
		if i.Name != nil {
			n = i.Name.Name
		}
		imported[n] = true
	}
	var out []string
	visitTargets(f, func(e ast.Expr) {
		if sel, ok := root(e).(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok && imported[id.Name] {
				out = append(out, id.Name+"."+sel.Sel.Name)
			}
		}
	})
	return out
}

// writesOf are the targets a file writes whose root is one of names.
func writesOf(f *ast.File, names map[string]bool) []string {
	if f == nil {
		return nil
	}
	var out []string
	visitTargets(f, func(e ast.Expr) {
		r := root(e)
		if sel, ok := r.(*ast.SelectorExpr); ok {
			r = sel.X // a field of the variable
		}
		if id, ok := r.(*ast.Ident); ok && names[id.Name] {
			out = append(out, id.Name)
		}
	})
	return out
}

// visitTargets calls fn with every expression a file writes: an assignment's left side, an increment's operand, the
// first argument of delete or clear, and the operand of a take of its address.
func visitTargets(f *ast.File, fn func(ast.Expr)) {
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			for _, l := range n.Lhs {
				fn(l)
			}
		case *ast.IncDecStmt:
			fn(n.X)
		case *ast.CallExpr:
			if id, ok := n.Fun.(*ast.Ident); ok && (id.Name == "delete" || id.Name == "clear") && len(n.Args) > 0 {
				fn(n.Args[0])
			}
		case *ast.UnaryExpr:
			if n.Op == token.AND {
				fn(n.X)
			}
		}
		return true
	})
}

// root is what a written expression writes into: past indexes, fields, dereferences and parentheses, down to a name
// or a package's name.
func root(e ast.Expr) ast.Expr {
	for {
		switch x := e.(type) {
		case *ast.IndexExpr:
			e = x.X
		case *ast.IndexListExpr:
			e = x.X
		case *ast.StarExpr:
			e = x.X
		case *ast.ParenExpr:
			e = x.X
		case *ast.SliceExpr:
			e = x.X
		case *ast.SelectorExpr:
			if _, ok := x.X.(*ast.Ident); ok {
				return x
			}
			e = x.X
		default:
			return e
		}
	}
}
