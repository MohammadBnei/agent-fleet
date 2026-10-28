package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// docPath is the document that describes core's configuration: the table under "### `core/`" in §7 of
// docs/ARCHITECTURE.md.
const docPath = "../../../docs/ARCHITECTURE.md"

// dynamicRead is the one read that has no literal name in config.go: a registered client's tokenRef, taken from the
// synced definitions and read through Config.TokenEnv at match time (docs/adr/0066 §3). The document lists it as a
// pattern.
const dynamicRead = "FLEET_CLIENT_TOKEN_<NAME>"

// documentedVars is the first cell of every table row of the core section (up to the next "### " heading, so the
// Auth subsection is in).
func documentedVars(doc string) []string {
	_, rest, ok := strings.Cut(doc, "\n### `core/`\n")
	if !ok {
		return nil
	}
	if i := strings.Index(rest, "\n### "); i >= 0 {
		rest = rest[:i]
	}
	row := regexp.MustCompile("^\\| `([A-Z][A-Z0-9_]*(?:<[A-Z]+>)?)` \\|")
	var out []string
	for _, line := range strings.Split(rest, "\n") {
		if m := row.FindStringSubmatch(line); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}

// readVars is what Load reads, by asking it: every name it passes to getenv (a recording getenv, nothing set), plus
// the dynamic one. A read behind a condition would be missed by that, so config.go is also scanned for upper-case
// literals passed as a call's first argument, and the two must agree.
func readVars(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	if _, err := Load(func(k string) string { seen[k] = true; return "" }); err != nil {
		t.Fatal(err)
	}
	f, err := parser.ParseFile(token.NewFileSet(), "config.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	upper := regexp.MustCompile(`^[A-Z][A-Z0-9_]+$`)
	literal := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok && len(c.Args) > 0 {
			if b, ok := c.Args[0].(*ast.BasicLit); ok && b.Kind == token.STRING {
				if s, _ := strconv.Unquote(b.Value); upper.MatchString(s) {
					literal[s] = true
				}
			}
		}
		return true
	})
	if !reflect.DeepEqual(seen, literal) {
		t.Fatalf("Load reads %v, config.go names %v: a read behind a condition, or a literal that is not a read", keys(seen), keys(literal))
	}
	return append(keys(seen), dynamicRead)
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// diff is the names in a and not in b.
func diff(a, b []string) []string {
	in := map[string]bool{}
	for _, s := range b {
		in[s] = true
	}
	var out []string
	for _, s := range a {
		if !in[s] {
			out = append(out, s)
		}
	}
	return out
}

// TestConfigDocumented: every variable core reads is a row of the table in docs/ARCHITECTURE.md, and every row is
// read by something.
func TestConfigDocumented(t *testing.T) {
	src, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatal(err)
	}
	documented := documentedVars(string(src))
	read := readVars(t)
	if missing := diff(read, documented); len(missing) > 0 {
		t.Errorf("read by core and not in the table of docs/ARCHITECTURE.md §7: %v", missing)
	}
	if stale := diff(documented, read); len(stale) > 0 {
		t.Errorf("in the table and read by nothing: %v", stale)
	}
	seen := map[string]bool{}
	for _, d := range documented {
		if seen[d] {
			t.Errorf("%s has two rows", d)
		}
		seen[d] = true
	}
}

// TestDocumentedVars: the table reader, and the check's three failures, on documents made for it (the positive
// control and the mutations of the real one).
func TestDocumentedVars(t *testing.T) {
	doc := "## 7\n\n### `core/`\n\n| Variable | Default |\n|---|---|\n| `A_B` | 1 |\n| `C` | x |\n| `FLEET_CLIENT_TOKEN_<NAME>` | none |\n" +
		"prose with `NOT_A_ROW` in it\n\n#### Auth\n\n| `D1` | y |\n\n### `provisioner/`\n\n| `OTHER` | z |\n"
	if got, want := documentedVars(doc), []string{"A_B", "C", "FLEET_CLIENT_TOKEN_<NAME>", "D1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("rows: %v, want %v", got, want)
	}
	if got := documentedVars("no core section"); got != nil {
		t.Errorf("no section: %v", got)
	}
	read := []string{"A_B", "C", "E"}
	if d := diff(read, []string{"A_B", "C"}); !reflect.DeepEqual(d, []string{"E"}) {
		t.Errorf("a variable missing from the table: %v", d)
	}
	if d := diff([]string{"A_B", "C", "GONE"}, read); !reflect.DeepEqual(d, []string{"GONE"}) {
		t.Errorf("a row nothing reads: %v", d)
	}
}
