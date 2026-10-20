package git

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteGenome(t *testing.T) {
	m := NewManager(t.TempDir())
	dir := m.GenomePath("s")

	first := []GenomeFile{
		{Path: ".claude/rules/20-style.md", Body: "short notes"},
		{Path: ".claude/skills/tidy/SKILL.md", Body: "---\nname: tidy\n---\n"},
	}
	if err := m.WriteGenome("s", first); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, ".claude/rules/20-style.md")); err != nil || string(b) != "short notes" {
		t.Fatalf("rules file = %q, %v", b, err)
	}

	// A re-render replaces the whole directory: a file dropped from the
	// render is gone, not left over from the last warm.
	if err := m.WriteGenome("s", []GenomeFile{{Path: ".claude/rules/90-fleet.md", Body: "fleet"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude/skills/tidy/SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("stale skill survived the re-render: %v", err)
	}
	for _, p := range []string{dir + ".new", dir + ".old"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s left behind: %v", p, err)
		}
	}

	for _, bad := range []string{
		".claude/settings.json", ".mcp.json", "../s/settings.json", ".claude/skills/../../x/SKILL.md",
		".claude/rules/style.md", "/etc/passwd", ".claude/skills/tidy/run.sh",
	} {
		if err := m.WriteGenome("s", []GenomeFile{{Path: bad, Body: "x"}}); err == nil {
			t.Errorf("WriteGenome accepted %q", bad)
		}
	}
	// A refused render leaves the last good genome in place.
	if b, err := os.ReadFile(filepath.Join(dir, ".claude/rules/90-fleet.md")); err != nil || string(b) != "fleet" {
		t.Fatalf("after a refused write, fleet file = %q, %v", b, err)
	}
	// Duplicate paths are refused, not silently overwritten.
	dup := []GenomeFile{{Path: "CLAUDE.md", Body: "a"}, {Path: "CLAUDE.md", Body: "b"}}
	if err := m.WriteGenome("s", dup); err == nil {
		t.Fatal("WriteGenome accepted a duplicate path")
	}

	if err := m.DeleteSessionDir("s"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("DeleteSessionDir left the genome: %v", err)
	}
}
