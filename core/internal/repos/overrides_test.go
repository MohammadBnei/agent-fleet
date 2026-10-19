package repos

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseOverrides(t *testing.T) {
	for _, empty := range []string{"", "# nothing\n", "repos: {}\n"} {
		m, err := ParseOverrides([]byte(empty))
		if err != nil || m == nil || len(m) != 0 {
			t.Fatalf("%q: (%v, %v), want an empty map", empty, m, err)
		}
	}
	m, err := ParseOverrides([]byte("repos:\n  a/b:\n    id: 7\n    clusterAccess: true\n    typo: 1\n"))
	if err != nil || m["a/b"] != (Override{ID: 7, ClusterAccess: true}) {
		t.Fatalf("unknown field: (%v, %v), want the entry kept", m, err)
	}
	if _, err := ParseOverrides([]byte("repos: [")); err == nil {
		t.Fatal("broken YAML parsed")
	}
	// A top-level typo must keep the last good copy, not mean "no overrides".
	if _, err := ParseOverrides([]byte("repo:\n  a/b: {id: 1}\n")); err == nil {
		t.Fatal("unknown top-level key parsed")
	}
	if m, _ := ParseOverrides([]byte("repos:\n  A/B: {id: 1}\n")); m["a/b"].ID != 1 {
		t.Fatal("keys not lowercased")
	}
}

// Missing before any good read: not ok, so core does not sync. Missing or
// broken after one: the last good copy. Empty: revoke everything.
func TestOverridesLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "overrides.yaml")
	o := &Overrides{Path: p}
	if _, ok := o.Load(); ok {
		t.Fatal("ok with no file ever read")
	}
	if err := os.WriteFile(p, []byte("repos:\n  a/b: {id: 1, clusterAccess: true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if m, ok := o.Load(); !ok || !m["a/b"].ClusterAccess {
		t.Fatalf("good file: (%v, %v)", m, ok)
	}
	if err := os.WriteFile(p, []byte("repos: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if m, ok := o.Load(); !ok || !m["a/b"].ClusterAccess {
		t.Fatalf("broken file dropped the last good copy: (%v, %v)", m, ok)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if m, ok := o.Load(); !ok || !m["a/b"].ClusterAccess {
		t.Fatalf("missing file dropped the last good copy: (%v, %v)", m, ok)
	}
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if m, ok := o.Load(); !ok || len(m) != 0 {
		t.Fatalf("empty file did not revoke: (%v, %v)", m, ok)
	}
}
