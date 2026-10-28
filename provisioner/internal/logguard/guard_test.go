package logguard

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Why this exists (mountain plan/15 §7: every log line has its level).
//
// An http.Server writes its own failures (a handler's panic, an Accept error)
// through the log package unless ErrorLog says otherwise. Once slog.SetDefault
// has run, the log package writes through slog's handler at INFO, so a panic
// arrives as an INFO line, not at ERROR where an error-rate alert counts it.
// Every server this module builds therefore says
//
//	ErrorLog: slog.NewLogLogger(slog.Default().Handler(), slog.LevelError)
//
// and so must be built after slog.SetDefault, since the handler is read when the
// literal is evaluated.
//
// This is a syntactic check (go/ast, no type information): it resolves the local
// names of net/http and log/slog per file, so an import alias is the same
// package, and it reads production files only (a _test.go file's throwaway
// server is not the fleet's). The accepted shape is exactly the one above;
// anything else, even a logger that would also work, fails, because "a logger
// that happens to work" is how a handler that discards gets in.
//
// What it sees, because it has no types: a literal whose type is spelled out as
// http.Server, and the elided element of a []http.Server, []*http.Server or map
// of either; new(http.Server); a zero http.Server value or field; a named type
// or alias of http.Server in this module; and any assignment to a .ErrorLog
// field (set it in the literal, nowhere else). What it cannot see: a server
// reached through another package's alias or a generic type parameter.

// minServers is the fewest http.Server literals each module must still be seen
// to build: the positive control, so a parse that stops finding them fails
// instead of passing on an empty set. Keyed by the last element of the module
// path, so the file is the same in every module.
var minServers = map[string]int{"core": 3, "provisioner": 1, "sidecar": 4, "executor": 1}

// importName is the name a file gives the package at path ("" if not imported,
// "." for a dot import, which the check refuses to follow).
func importName(f *ast.File, path, def string) string {
	for _, im := range f.Imports {
		p, err := strconv.Unquote(im.Path.Value)
		if err != nil || p != path {
			continue
		}
		if im.Name != nil {
			return im.Name.Name
		}
		return def
	}
	return ""
}

// isSel reports whether e is the selector pkg.name.
func isSel(e ast.Expr, pkg, name string) bool {
	s, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := s.X.(*ast.Ident)
	return ok && x.Name == pkg && s.Sel.Name == name
}

// serverType reports whether e is http.Server or *http.Server.
func serverType(e ast.Expr, httpName string) bool {
	if st, ok := e.(*ast.StarExpr); ok {
		e = st.X
	}
	return isSel(e, httpName, "Server")
}

// isCall reports whether e is a call of pkg.name with n arguments.
func isCall(e ast.Expr, pkg, name string, n int) (*ast.CallExpr, bool) {
	c, ok := e.(*ast.CallExpr)
	if !ok || len(c.Args) != n || !isSel(c.Fun, pkg, name) {
		return nil, false
	}
	return c, true
}

// errorLogOK reports whether v is exactly slog.NewLogLogger(slog.Default().Handler(), slog.LevelError).
func errorLogOK(v ast.Expr, slogName string) bool {
	c, ok := isCall(v, slogName, "NewLogLogger", 2)
	if !ok {
		return false
	}
	h, ok := c.Args[0].(*ast.CallExpr) // <x>.Handler()
	if !ok || len(h.Args) != 0 {
		return false
	}
	hs, ok := h.Fun.(*ast.SelectorExpr)
	if !ok || hs.Sel.Name != "Handler" {
		return false
	}
	if _, ok := isCall(hs.X, slogName, "Default", 0); !ok {
		return false
	}
	return isSel(c.Args[1], slogName, "LevelError")
}

type finding struct {
	pos token.Position
	msg string
}

func (f finding) String() string { return fmt.Sprintf("%s: %s", f.pos, f.msg) }

// report is what lint saw: the problems, and how many http.Server literals it
// looked at (the positive control).
type report struct {
	problems []finding
	literals int
}

// lint checks the production sources in files (path -> source). Paths only
// matter for grouping files into packages (by directory) and for messages.
func lint(files map[string]string) report {
	fset := token.NewFileSet()
	var r report
	names := make([]string, 0, len(files))
	for p := range files {
		names = append(names, p)
	}
	sort.Strings(names)

	type pkgFile struct {
		f    *ast.File
		http string
		slog string
	}
	byDir := map[string][]pkgFile{}
	for _, p := range names {
		f, err := parser.ParseFile(fset, p, files[p], parser.SkipObjectResolution)
		if err != nil {
			r.problems = append(r.problems, finding{token.Position{Filename: p}, "does not parse: " + err.Error()})
			continue
		}
		byDir[filepath.Dir(p)] = append(byDir[filepath.Dir(p)], pkgFile{f, importName(f, "net/http", "http"), importName(f, "log/slog", "slog")})
	}
	bad := func(pos token.Pos, format string, a ...any) {
		r.problems = append(r.problems, finding{fset.Position(pos), fmt.Sprintf(format, a...)})
	}

	dirs := make([]string, 0, len(byDir))
	for d := range byDir {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		// literalFuncs: the plain functions of this package that contain a server
		// literal, with the position of every one of them (main is ordered literal by literal).
		literalFuncs := map[string][]token.Pos{}
		hasServer := false
		for _, pf := range byDir[dir] {
			if pf.http == "." {
				bad(pf.f.Pos(), `dot import of "net/http": the check cannot see its Server`)
				continue
			}
			if pf.http == "" || pf.http == "_" {
				continue
			}
			for _, d := range pf.f.Decls {
				fd, isFn := d.(*ast.FuncDecl)
				// server checks one http.Server composite literal, however its type is written.
				server := func(n *ast.CompositeLit) {
					r.literals++
					hasServer = true
					if !isFn || fd.Recv != nil || fd.Body == nil {
						bad(n.Pos(), "an http.Server literal outside a plain function: build it in a function the order check can follow")
					} else {
						literalFuncs[fd.Name.Name] = append(literalFuncs[fd.Name.Name], n.Pos())
					}
					var logv ast.Expr
					for _, e := range n.Elts {
						if kv, ok := e.(*ast.KeyValueExpr); ok {
							if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "ErrorLog" {
								logv = kv.Value
							}
						}
					}
					switch {
					case logv == nil:
						bad(n.Pos(), "an http.Server without ErrorLog: net/http's own lines would reach the log package at INFO")
					case pf.slog == "" || pf.slog == "." || pf.slog == "_" || !errorLogOK(logv, pf.slog):
						bad(logv.Pos(), "ErrorLog is not slog.NewLogLogger(slog.Default().Handler(), slog.LevelError)")
					}
				}
				// elided checks the elements of a composite literal of []http.Server,
				// []*http.Server or a map of either, whose element type is not written.
				elided := func(n *ast.CompositeLit) {
					var elem ast.Expr
					switch t := n.Type.(type) {
					case *ast.ArrayType:
						elem = t.Elt
					case *ast.MapType:
						elem = t.Value
					}
					if elem == nil || !serverType(elem, pf.http) {
						return
					}
					for _, e := range n.Elts {
						if kv, ok := e.(*ast.KeyValueExpr); ok {
							e = kv.Value
						}
						if c, ok := e.(*ast.CompositeLit); ok && c.Type == nil {
							server(c)
						}
					}
				}
				ast.Inspect(d, func(n ast.Node) bool {
					switch n := n.(type) {
					case *ast.CompositeLit:
						if isSel(n.Type, pf.http, "Server") {
							server(n)
						} else if n.Type != nil {
							elided(n)
						}
					case *ast.AssignStmt:
						for _, l := range n.Lhs {
							if s, ok := l.(*ast.SelectorExpr); ok && s.Sel.Name == "ErrorLog" {
								bad(n.Pos(), "ErrorLog is assigned after the server is built: set it in the literal, where the check reads it")
							}
						}
					case *ast.TypeSpec:
						if serverType(n.Type, pf.http) {
							bad(n.Pos(), "a named type or alias of http.Server hides its literals from the check: use http.Server itself")
						}
					case *ast.CallExpr:
						if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "new" && len(n.Args) == 1 && isSel(n.Args[0], pf.http, "Server") {
							bad(n.Pos(), "new(http.Server) has no ErrorLog: use a literal that sets it")
						}
					case *ast.ValueSpec:
						if n.Type != nil && isSel(n.Type, pf.http, "Server") {
							bad(n.Pos(), "a zero http.Server value has no ErrorLog: use a literal that sets it")
						}
					case *ast.Field:
						if isSel(n.Type, pf.http, "Server") {
							bad(n.Pos(), "an http.Server held by value has no ErrorLog: hold a *http.Server built by a literal")
						}
					}
					return true
				})
			}
		}
		if !hasServer {
			continue
		}

		// Order: the handler is read when the literal is evaluated, so slog.SetDefault must already have run.
		// Every function with a literal is main itself (after the SetDefault) or is used only from main, after it.
		var mainDecl *ast.FuncDecl
		var mainSlog string
		for _, pf := range byDir[dir] {
			if pf.f.Name.Name != "main" {
				continue
			}
			for _, d := range pf.f.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "main" && fd.Body != nil {
					mainDecl, mainSlog = fd, pf.slog
				}
			}
		}
		if mainDecl == nil {
			bad(byDir[dir][0].f.Pos(), "%s builds an http.Server outside package main: the order against slog.SetDefault cannot be checked", dir)
			continue
		}
		setDefault := token.NoPos
		ast.Inspect(mainDecl.Body, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok && mainSlog != "" {
				if _, ok := isCall(c, mainSlog, "SetDefault", 1); ok && (setDefault == token.NoPos || c.Pos() < setDefault) {
					setDefault = c.Pos()
				}
			}
			return true
		})
		if setDefault == token.NoPos {
			bad(mainDecl.Pos(), "main builds http.Servers but never calls slog.SetDefault")
			continue
		}
		lits := make([]string, 0, len(literalFuncs))
		for name := range literalFuncs {
			lits = append(lits, name)
		}
		sort.Strings(lits)
		for _, name := range lits {
			if name == "main" {
				for _, pos := range literalFuncs[name] {
					if pos < setDefault {
						bad(pos, "an http.Server is built in main before slog.SetDefault")
					}
				}
				continue
			}
			uses := 0
			for _, pf := range byDir[dir] {
				for _, d := range pf.f.Decls {
					fd, isFn := d.(*ast.FuncDecl)
					ast.Inspect(d, func(n ast.Node) bool {
						id, ok := n.(*ast.Ident)
						if !ok || id.Name != name {
							return true
						}
						if isFn && fd.Name == id {
							return true // its own declaration
						}
						uses++
						if !isFn || fd != mainDecl {
							bad(id.Pos(), "%s builds an http.Server and is used outside main: the order against slog.SetDefault is no longer a main-only fact", name)
						} else if id.Pos() < setDefault {
							bad(id.Pos(), "%s builds an http.Server and is used before slog.SetDefault", name)
						}
						return true
					})
				}
			}
			if uses == 0 {
				bad(literalFuncs[name][0], "%s builds an http.Server and nothing in main uses it", name)
			}
		}
	}
	return r
}

// moduleName is the last element of the module path in go.mod.
func moduleName(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	line, _, _ := strings.Cut(string(b), "\n")
	return filepath.Base(strings.TrimSpace(strings.TrimPrefix(line, "module")))
}

// readModule reads every production .go file under root.
func readModule(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n == "testdata" || n == "vendor" || n == "node_modules" || strings.HasPrefix(n, ".") && p != root {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[p] = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestHTTPServersLogAtError: every http.Server this module builds sets ErrorLog
// to slog's default handler at LevelError, after slog.SetDefault.
func TestHTTPServersLogAtError(t *testing.T) {
	root := filepath.Join("..", "..")
	name := moduleName(t, root)
	want, ok := minServers[name]
	if !ok {
		t.Fatalf("module %q is not in minServers: say how many http.Servers it builds (0 is fine)", name)
	}
	r := lint(readModule(t, root))
	for _, p := range r.problems {
		t.Error(p)
	}
	if r.literals < want {
		t.Errorf("%d http.Server literals seen, want at least %d: the check no longer sees them", r.literals, want)
	}
}

// good is a minimal correct main: the positive control for the cases below, each
// of which breaks one thing in it.
const good = `package main

import (
	"log/slog"
	"net/http"
	"os"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	run()
}

func run() {
	_ = &http.Server{Addr: ":1", ErrorLog: slog.NewLogLogger(slog.Default().Handler(), slog.LevelError)}
}
`

// TestLintCatchesEachBreak: the checker itself, on source text. good passes;
// each mutation of it fails with the reason it should, so a checker that stops
// looking cannot pass here.
func TestLintCatchesEachBreak(t *testing.T) {
	run := func(src string) report { return lint(map[string]string{"cmd/x/main.go": src}) }

	if r := run(good); len(r.problems) != 0 || r.literals != 1 {
		t.Fatalf("positive control: problems %v, literals %d, want none and 1", r.problems, r.literals)
	}
	aliased := strings.NewReplacer(`"log/slog"`, `sl "log/slog"`, `"net/http"`, `h "net/http"`, "slog.", "sl.", "http.", "h.").Replace(good)
	if r := run(aliased); len(r.problems) != 0 || r.literals != 1 {
		t.Fatalf("positive control (aliased imports): problems %v, literals %d", r.problems, r.literals)
	}
	// Two packages are checked apart: the same names in each, both correct.
	if r := lint(map[string]string{"cmd/x/main.go": good, "cmd/y/main.go": good}); len(r.problems) != 0 || r.literals != 2 {
		t.Fatalf("positive control (two packages): problems %v, literals %d, want none and 2", r.problems, r.literals)
	}

	const logger = `slog.NewLogLogger(slog.Default().Handler(), slog.LevelError)`
	const body = `_ = &http.Server{Addr: ":1", ErrorLog: ` + logger + `}`
	const setDefault = "\tslog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))\n"

	// Positive controls for the forms whose type is not spelled out as http.Server.
	for name, lit := range map[string]string{
		"slice of pointers": `_ = []*http.Server{{Addr: ":1", ErrorLog: ` + logger + `}, {ErrorLog: ` + logger + `}}`,
		"slice of values":   `_ = []http.Server{{ErrorLog: ` + logger + `}}`,
		"map":               `_ = map[string]*http.Server{"a": {ErrorLog: ` + logger + `}}`,
	} {
		want := 1
		if name == "slice of pointers" {
			want = 2
		}
		if r := run(strings.Replace(good, body, lit, 1)); len(r.problems) != 0 || r.literals != want {
			t.Errorf("positive control (%s): problems %v, literals %d, want none and %d", name, r.problems, r.literals, want)
		}
	}
	// Two literals in main, both after the set, is fine: the order is checked for each.
	two := strings.Replace(good, "\trun()\n", "\t_ = &http.Server{ErrorLog: "+logger+"}\n\trun()\n", 1)
	if r := run(two); len(r.problems) != 0 || r.literals != 2 {
		t.Errorf("positive control (two literals in main): problems %v, literals %d, want none and 2", r.problems, r.literals)
	}

	cases := []struct {
		name, src, want string
	}{
		{"field missing", strings.Replace(good, `, ErrorLog: `+logger, "", 1), "without ErrorLog"},
		{"positional literal", strings.Replace(good, `&http.Server{Addr: ":1", ErrorLog: `+logger+`}`, `&http.Server{"x"}`, 1), "without ErrorLog"},
		{"level warn", strings.Replace(good, "slog.LevelError", "slog.LevelWarn", 1), "is not slog.NewLogLogger"},
		{"level info", strings.Replace(good, "slog.LevelError", "slog.LevelInfo", 1), "is not slog.NewLogLogger"},
		{"discarding handler", strings.Replace(good, "slog.Default().Handler()", "slog.DiscardHandler", 1), "is not slog.NewLogLogger"},
		{"handler to io.Discard", strings.Replace(good, "slog.Default().Handler()", "slog.NewTextHandler(io.Discard, nil)", 1), "is not slog.NewLogLogger"},
		{"nil logger", strings.Replace(good, logger, "nil", 1), "is not slog.NewLogLogger"},
		{"log package logger", strings.Replace(good, logger, `log.New(os.Stderr, "", 0)`, 1), "is not slog.NewLogLogger"},
		{"new(http.Server)", strings.Replace(good, `_ = &http.Server{Addr: ":1", ErrorLog: `+logger+`}`, "_ = new(http.Server)", 1), "new(http.Server)"},
		{"zero value", strings.Replace(good, `_ = &http.Server{Addr: ":1", ErrorLog: `+logger+`}`, "var s http.Server\n\t_ = &s", 1), "zero http.Server"},
		{"set after the server is built", strings.Replace(strings.Replace(good, "\tslog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))\n\trun()\n", "\trun()\n\tslog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))\n", 1), "x", "x", 1), "used before slog.SetDefault"},
		{"never set", strings.Replace(good, "\tslog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))\n", "", 1), "never calls slog.SetDefault"},
		{"built in main before the set", strings.Replace(good, "\tslog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))\n\trun()\n",
			"\t_ = &http.Server{ErrorLog: "+logger+"}\n\tslog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))\n\trun()\n", 1), "before slog.SetDefault"},
		{"two literals in main, the first before the set", strings.Replace(good, setDefault+"\trun()\n",
			"\t_ = &http.Server{ErrorLog: "+logger+"}\n"+setDefault+"\t_ = &http.Server{ErrorLog: "+logger+"}\n\trun()\n", 1), "before slog.SetDefault"},
		{"elided literal without ErrorLog", strings.Replace(good, body, `_ = []*http.Server{{Addr: ":1"}}`, 1), "without ErrorLog"},
		{"elided literal, wrong level", strings.Replace(good, body, `_ = []*http.Server{{ErrorLog: `+strings.Replace(logger, "LevelError", "LevelWarn", 1)+`}}`, 1), "is not slog.NewLogLogger"},
		{"elided map literal without ErrorLog", strings.Replace(good, body, `_ = map[string]http.Server{"a": {}}`, 1), "without ErrorLog"},
		{"ErrorLog assigned later", strings.Replace(good, body, "s := &http.Server{ErrorLog: "+logger+"}\n\ts.ErrorLog = nil\n\t_ = s", 1), "assigned after"},
		{"alias of http.Server", good + "\ntype srv = http.Server\n", "named type or alias"},
		{"defined type of http.Server", good + "\ntype srv http.Server\n", "named type or alias"},
		{"builder also called elsewhere", good + "\nfunc other() { run() }\n", "used outside main"},
		{"builder never used", strings.Replace(good, "\trun()\n", "", 1), "nothing in main uses it"},
		{"dot import", strings.Replace(good, `"net/http"`, `. "net/http"`, 1), "dot import"},
	}
	for _, c := range cases {
		if c.src == good {
			t.Errorf("%s: the mutation did not change the source", c.name)
			continue
		}
		r := run(c.src)
		found := false
		for _, p := range r.problems {
			found = found || strings.Contains(p.msg, c.want)
		}
		if !found {
			t.Errorf("%s: want a problem containing %q, got %v", c.name, c.want, r.problems)
		}
	}

	// A server outside package main cannot be ordered against SetDefault.
	lib := strings.Replace(strings.Replace(good, "package main", "package lib", 1), "func main()", "func notmain()", 1)
	if r := lint(map[string]string{"internal/x/x.go": lib}); len(r.problems) == 0 {
		t.Error("an http.Server outside package main should be reported")
	}
}
