package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// envReaders are what reads or changes the process's environment (core's
// internal/config/env_test.go, atlas's TestEnvBan): os's and syscall's own, and
// the ones that read a variable on the caller's behalf. A package listed with no
// names is refused whole: os/exec reads PATH and hands the environment on,
// os/user reads HOME and USER. The check is syntactic: every use of a listed
// name is a selector on its import's name, or a dot import, which is refused;
// cgo and go:linkname are refused too.
//
// ponytail: this file is twinned in core/internal/config,
// provisioner/internal/config, sidecar/internal/config and
// executor/internal/config: they are separate go.work modules with no shared
// package, so a fix to the checker is made in all of them.
var envReaders = map[string][]string{
	"os": {"Getenv", "LookupEnv", "Environ", "ExpandEnv", "Setenv", "Unsetenv", "Clearenv", "UserHomeDir", "UserCacheDir",
		"UserConfigDir", "TempDir", "MkdirTemp", "CreateTemp", "Getwd"},
	"syscall":               {"Getenv", "Environ", "Setenv", "Unsetenv", "Clearenv"},
	"golang.org/x/sys/unix": {"Getenv", "Environ", "Setenv", "Unsetenv", "Clearenv"}, // in this module's dependency graph
	"net/http":              {"ProxyFromEnvironment"},
	"os/exec":               nil,
	"os/user":               nil,
}

// envReadsAllowed is what may read the environment, by file, and what each may
// use (mountain plan/15 §5: one Config per binary). A file listed here must use
// exactly these and nothing else: a use that is gone fails too, which is the
// positive control. The libraries' own reads are theirs: client-go's
// InClusterConfig, grpc's and net/http's proxy variables.
var envReadsAllowed = map[string][]string{
	// The one reader: FromProcess hands os.Getenv to Load.
	"internal/config/config.go": {"os.Getenv"},
	// git runs with the process's environment on purpose: PATH to find git, HOME
	// and the ambient git auth the "using ambient git auth" warning names, and
	// the App's credential variables appended to it. Child process, not config.
	"internal/git/git.go": {"import os/exec", "os.Environ"},
	// The genome's staging directory is created inside the session's own
	// claude-home, never in $TMPDIR: the first argument is a path, not "".
	"internal/git/genome.go": {"os.MkdirTemp"},
}

// envUses lists the names in src that read the environment, as "pkg.Name" or
// "import pkg". os.MkdirTemp and os.CreateTemp read $TMPDIR when their first
// argument is "": a call whose first argument is anything else is reported plain
// ("os.MkdirTemp"), and any other form (an empty literal, a function value) as
// "os.MkdirTemp(unchecked)", which nothing allows.
func envUses(name string, src []byte) ([]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), name, src, parser.SkipObjectResolution|parser.ParseComments)
	if err != nil {
		return nil, err
	}
	local := map[string]string{}
	var uses []string
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			if strings.HasPrefix(c.Text, "//go:linkname") {
				uses = append(uses, "go:linkname")
			}
		}
	}
	for _, im := range f.Imports {
		p, _ := strconv.Unquote(im.Path.Value)
		names, ok := envReaders[p]
		if p == "C" || (ok && names == nil) {
			uses = append(uses, "import "+p)
			continue
		}
		if !ok {
			continue
		}
		n := path.Base(p)
		if im.Name != nil {
			n = im.Name.Name
		}
		if n == "." {
			uses = append(uses, "import . "+p)
			continue
		}
		local[n] = p
	}
	// selector reports the "pkg.Name" a selector names, when pkg is a listed import.
	selector := func(n ast.Expr) (pkg, sel string, ok bool) {
		s, isSel := n.(*ast.SelectorExpr)
		if !isSel {
			return "", "", false
		}
		id, isID := s.X.(*ast.Ident)
		if !isID || local[id.Name] == "" || !slices.Contains(envReaders[local[id.Name]], s.Sel.Name) {
			return "", "", false
		}
		return local[id.Name], s.Sel.Name, true
	}
	tempCall := map[ast.Expr]bool{} // the selectors already judged as a call's function
	ast.Inspect(f, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if pkg, sel, ok := selector(c.Fun); ok && pkg == "os" && (sel == "MkdirTemp" || sel == "CreateTemp") {
				tempCall[c.Fun] = true
				if !emptyDir(c.Args) {
					uses = append(uses, "os."+sel)
				} else {
					uses = append(uses, "os."+sel+"(unchecked)")
				}
			}
		}
		if e, ok := n.(ast.Expr); ok && !tempCall[e] {
			if pkg, sel, ok := selector(e); ok {
				if pkg == "os" && (sel == "MkdirTemp" || sel == "CreateTemp") {
					sel += "(unchecked)"
				}
				uses = append(uses, pkg+"."+sel)
			}
		}
		return true
	})
	return uses, nil
}

// emptyDir reports whether a MkdirTemp/CreateTemp call's first argument is
// missing or the empty string literal, "" or “: the forms that read $TMPDIR.
func emptyDir(args []ast.Expr) bool {
	if len(args) == 0 {
		return true
	}
	lit, ok := args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	v, err := strconv.Unquote(lit.Value)
	return err != nil || v == ""
}

// set is uses without repeats, sorted.
func set(uses []string) []string { return slices.Compact(slices.Sorted(slices.Values(uses))) }

// TestEnvBan: no non-test file of this module reads the environment but the ones
// envReadsAllowed names, and each of those only in the way it names (tests are
// exempt).
func TestEnvBan(t *testing.T) {
	root := "../.."
	seen := map[string]bool{}
	files := map[string]bool{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		seen[path.Dir(rel)] = true // a package of tests only is walked too: it has nothing to check
		if strings.HasSuffix(p, "_test.go") {
			return nil
		}
		files[rel] = true
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		uses, err := envUses(rel, src)
		if err != nil {
			return err
		}
		got, want := set(uses), set(envReadsAllowed[rel])
		switch {
		case len(want) > 0 && len(got) == 0:
			t.Errorf("%s reads no environment: the check no longer sees it", rel) // the positive control
		case !slices.Equal(got, want):
			t.Errorf("%s reads the environment as %v, and only %v is allowed", rel, got, want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for rel := range envReadsAllowed {
		if !files[rel] {
			t.Errorf("%s is allowed to read the environment and is not a file of this module", rel)
		}
	}
	cmd := exec.Command("go", "list", "-f", "{{.Dir}}", "./...")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(root)
	for _, dir := range strings.Fields(string(out)) {
		if rel, _ := filepath.Rel(abs, dir); !seen[filepath.ToSlash(rel)] {
			t.Errorf("package %s was not walked", rel)
		}
	}
}

// TestEnvUses: each form the ban sees, and what it leaves alone.
func TestEnvUses(t *testing.T) {
	for _, c := range []struct {
		src  string
		want []string
	}{
		{`import "os"; var _ = os.Getenv("X")`, []string{"os.Getenv"}},
		{`import "os"; var f = os.LookupEnv`, []string{"os.LookupEnv"}},
		{`import o "os"; func g() { o.Environ() }`, []string{"os.Environ"}},
		{`import . "os"; var _ = Getenv("X")`, []string{"import . os"}},
		{`import "os"; var _ = os.ExpandEnv("$X")`, []string{"os.ExpandEnv"}},
		{`import "os"; var _, _ = os.UserHomeDir()`, []string{"os.UserHomeDir"}},
		{`import "syscall"; var _, _ = syscall.Getenv("X")`, []string{"syscall.Getenv"}},
		{`import sc "syscall"; var _ = sc.Environ`, []string{"syscall.Environ"}},
		{`import "golang.org/x/sys/unix"; var _ = unix.Getenv`, []string{"golang.org/x/sys/unix.Getenv"}},
		{`import u "golang.org/x/sys/unix"; var _, _ = u.Getenv("X")`, []string{"golang.org/x/sys/unix.Getenv"}},
		{`import "os"; var _ = os.TempDir()`, []string{"os.TempDir"}},
		{`import "os"; var _, _ = os.MkdirTemp("", "x")`, []string{"os.MkdirTemp(unchecked)"}},
		{`import "os"; var _, _ = os.CreateTemp("", "x")`, []string{"os.CreateTemp(unchecked)"}},
		{"import \"os\"; var _, _ = os.MkdirTemp(``, \"x\")", []string{"os.MkdirTemp(unchecked)"}},
		{`import "os"; var _, _ = os.MkdirTemp("/var/x", "x")`, []string{"os.MkdirTemp"}},
		{`import "os"; func f(d string) { _, _ = os.CreateTemp(d, "x") }`, []string{"os.CreateTemp"}},
		{`import "os"; var f = os.MkdirTemp`, []string{"os.MkdirTemp(unchecked)"}},
		{`import "os"; var _, _ = os.MkdirTemp()`, []string{"os.MkdirTemp(unchecked)"}},
		{`import "os"; var _, _ = os.Getwd()`, []string{"os.Getwd"}},
		{`import "net/http"; var _ = http.ProxyFromEnvironment`, []string{"net/http.ProxyFromEnvironment"}},
		{`import h "net/http"; var _ = h.ProxyFromEnvironment`, []string{"net/http.ProxyFromEnvironment"}},
		{`import "os/exec"; var _ = exec.Command`, []string{"import os/exec"}},
		{`import u "os/user"; var _, _ = u.Current()`, []string{"import os/user"}},
		{`import "C"`, []string{"import C"}},
		{"import _ \"unsafe\"\n//go:linkname getenv syscall.Getenv\nfunc getenv(string) (string, bool)", []string{"go:linkname"}},
		{`import "os"; var _ = os.Args; var _ = os.Stderr; var _ = os.Exit`, nil},
		{`import "os"; var _ = os.ReadFile`, nil},
		{`import "net/http"; var _ = http.StatusOK`, nil},
		{`type t struct{ Getenv func(string) string }; var os t; var _ = os.Getenv`, nil}, // no import: not os
	} {
		got, err := envUses("x.go", []byte("package x\n"+c.src))
		if err != nil {
			t.Fatalf("%s: %v", c.src, err)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.src, got, c.want)
		}
	}
}
