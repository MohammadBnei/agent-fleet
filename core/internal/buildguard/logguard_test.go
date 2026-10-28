package buildguard

import (
	"os"
	"path/filepath"
	"testing"
)

// logguardFiles are the guard's files, one copy per Go module (a module cannot
// import another module's test). Each module's own test only proves its own
// servers; this proves the four are the same guard, so a fix to one reaches
// all. Reads files outside this module: run with -count=1.
var logguardFiles = []string{"doc.go", "guard_test.go"}

var logguardModules = []string{"core", "provisioner", "sidecar", "executor"}

// logguardDrift names each copy that differs from the first module's, for the
// file given.
func logguardDrift(copies map[string]string) []string {
	var drift []string
	for _, m := range logguardModules[1:] {
		if copies[m] != copies[logguardModules[0]] {
			drift = append(drift, m)
		}
	}
	return drift
}

func TestLogGuardCopiesAreIdentical(t *testing.T) {
	root := repoRoot(t)
	for _, name := range logguardFiles {
		copies := map[string]string{}
		for _, m := range logguardModules {
			b, err := os.ReadFile(filepath.Join(root, m, "internal", "logguard", name))
			if err != nil {
				t.Fatalf("%s: %v", m, err)
			}
			copies[m] = string(b)
		}
		if d := logguardDrift(copies); len(d) != 0 {
			t.Errorf("internal/logguard/%s in %v differs from core's: the guard is one file in four modules, edit them together (cp core/internal/logguard/%s <module>/internal/logguard/)", name, d, name)
		}
	}
}

// TestLogGuardDriftIsSeen: the comparison above notices a changed copy (the
// mutation) and passes four equal ones (the positive control).
func TestLogGuardDriftIsSeen(t *testing.T) {
	same := map[string]string{"core": "a", "provisioner": "a", "sidecar": "a", "executor": "a"}
	if d := logguardDrift(same); len(d) != 0 {
		t.Errorf("four equal copies reported as drift: %v", d)
	}
	for _, m := range logguardModules[1:] {
		changed := map[string]string{"core": "a", "provisioner": "a", "sidecar": "a", "executor": "a"}
		changed[m] = "b"
		if d := logguardDrift(changed); len(d) != 1 || d[0] != m {
			t.Errorf("a changed %s copy reported as %v", m, d)
		}
	}
	changed := map[string]string{"core": "b", "provisioner": "a", "sidecar": "a", "executor": "a"}
	if d := logguardDrift(changed); len(d) != 3 {
		t.Errorf("a changed core copy should differ from all three others, got %v", d)
	}
}
