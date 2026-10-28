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

// envReaders are what reads or changes the process's environment (as atlas's TestEnvBan, mountain env_test.go): os's
// and syscall's own, and the ones that read a variable on the caller's behalf. A package listed with no names is
// refused whole: os/exec reads PATH and hands the environment on, os/user reads HOME and USER. The check is
// syntactic: every use of a listed name is a selector on its import's name, or a dot import, which is refused; cgo
// and go:linkname are refused too.
var envReaders = map[string][]string{
	"os": {"Getenv", "LookupEnv", "Environ", "ExpandEnv", "Setenv", "Unsetenv", "Clearenv", "UserHomeDir", "UserCacheDir",
		"UserConfigDir", "TempDir", "MkdirTemp", "CreateTemp", "Getwd"},
	"syscall":               {"Getenv", "Environ", "Setenv", "Unsetenv", "Clearenv"},
	"golang.org/x/sys/unix": {"Getenv", "Environ", "Setenv", "Unsetenv", "Clearenv"}, // in core's dependency graph
	"net/http":              {"ProxyFromEnvironment"},
	"os/exec":               nil,
	"os/user":               nil,
}

// envReadsAllowed is what reads the environment (mountain plan/15 §5: one Config per binary): config.go, and dbtest's
// test DSN (a package only tests import). The libraries' own reads are theirs: the AWS SDK's AWS_* (filestore's
// LoadDefaultConfig), pgx's PG* defaults, net/http's proxy variables.
var envReadsAllowed = map[string]bool{"internal/config/config.go": true, "internal/dbtest/dbtest.go": true}

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
	ast.Inspect(f, func(n ast.Node) bool {
		if s, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := s.X.(*ast.Ident); ok && local[id.Name] != "" && slices.Contains(envReaders[local[id.Name]], s.Sel.Name) {
				uses = append(uses, local[id.Name]+"."+s.Sel.Name)
			}
		}
		return true
	})
	return uses, nil
}

// TestEnvBan: no non-test file of core's module but envReadsAllowed reads the environment (tests are exempt).
func TestEnvBan(t *testing.T) {
	root := "../.."
	seen := map[string]bool{}
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
		seen[path.Dir(rel)] = true // a package of tests only (buildguard) is walked too: it has nothing to check
		if strings.HasSuffix(p, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		uses, err := envUses(rel, src)
		if err != nil {
			return err
		}
		switch {
		case envReadsAllowed[rel] && len(uses) == 0:
			t.Errorf("%s reads no environment: the check no longer sees it", rel) // the positive control
		case !envReadsAllowed[rel] && len(uses) > 0:
			t.Errorf("%s reads the environment (%s): only config.go does", rel, strings.Join(uses, ", "))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
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
		{`import "os"; var _, _ = os.UserHomeDir()`, []string{"os.UserHomeDir"}},
		{`import "syscall"; var _, _ = syscall.Getenv("X")`, []string{"syscall.Getenv"}},
		{`import "golang.org/x/sys/unix"; var _ = unix.Getenv`, []string{"golang.org/x/sys/unix.Getenv"}},
		{`import "os"; var _ = os.TempDir()`, []string{"os.TempDir"}},
		{`import "os"; var _, _ = os.MkdirTemp("", "x")`, []string{"os.MkdirTemp"}},
		{`import "os"; var _, _ = os.CreateTemp("", "x")`, []string{"os.CreateTemp"}},
		{`import "os"; var _, _ = os.Getwd()`, []string{"os.Getwd"}},
		{`import "net/http"; var _ = http.ProxyFromEnvironment`, []string{"net/http.ProxyFromEnvironment"}},
		{`import h "net/http"; var _ = h.ProxyFromEnvironment`, []string{"net/http.ProxyFromEnvironment"}},
		{`import "os/exec"; var _ = exec.Command`, []string{"import os/exec"}},
		{`import u "os/user"; var _, _ = u.Current()`, []string{"import os/user"}},
		{`import "C"`, []string{"import C"}},
		{"import _ \"unsafe\"\n//go:linkname getenv syscall.Getenv\nfunc getenv(string) (string, bool)", []string{"go:linkname"}},
		{`import "os"; var _ = os.Args; var _ = os.Stderr`, nil},
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
