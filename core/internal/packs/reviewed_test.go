package packs

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseReviewed(t *testing.T) {
	good, bad := ParseReviewed(map[string][]byte{
		"definitions/a.yaml":                  []byte("a"),
		listedMarker:                          nil,
		"packs/fleet/PACK.md":                 []byte("fleet rules"),
		"packs/fleet/skills/journal/SKILL.md": []byte("---\nname: journal\nallowed-tools: Bash\n---\nj"),
		"packs/repo/PACK.md":                  []byte("repo rules"),
		"packs/repo/mcp.json":                 []byte("{}"),
		"packs/other/PACK.md":                 []byte("x"),
	})
	if len(good) != 1 || good["fleet"].Text != "fleet rules" {
		t.Fatalf("good = %+v", good)
	}
	if len(bad) != 2 || bad["repo"] == "" || bad["other"] == "" {
		t.Fatalf("bad = %v, want repo (mcp file) and other (not read yet)", bad)
	}
	// A reviewed SKILL.md is used as written: review is what allows its keys.
	if s := good["fleet"].Skills; len(s) != 1 || s[0].Name != "journal" || !strings.HasPrefix(s[0].Body, "---\n") {
		t.Fatalf("fleet skills = %+v", s)
	}

	for name, files := range map[string]map[string][]byte{
		"marker":  {"packs/fleet/.invalid.md": []byte("packs/fleet/x is not a regular file")},
		"nul":     {"packs/fleet/PACK.md": []byte("a\x00b")},
		"utf8":    {"packs/fleet/PACK.md": []byte{0xff, 0xfe}},
		"import":  {"packs/fleet/PACK.md": []byte("see @/workspace/x")},
		"size":    {"packs/fleet/PACK.md": []byte(strings.Repeat("a", maxReviewedText+1))},
		"skillnm": {"packs/fleet/skills/Bad/SKILL.md": []byte("x")},
	} {
		if good, bad := ParseReviewed(files); len(good) != 0 || bad["fleet"] == "" {
			t.Errorf("%s: good %v bad %v, want fleet refused", name, good, bad)
		}
	}
}

// The repo's own packs/ must parse, or a merge ships a pack that fails at sync
// and pods keep the last good copy without anyone noticing.
func TestRepoPacksParse(t *testing.T) {
	files := map[string][]byte{listedMarker: nil}
	root := "../../.."
	err := filepath.WalkDir(filepath.Join(root, "packs"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		rel, _ := filepath.Rel(root, p)
		files[filepath.ToSlash(rel)] = b
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	good, bad := ParseReviewed(files)
	if len(bad) != 0 || good["fleet"].Text == "" || good["repo"].Text == "" {
		t.Fatalf("good %v, bad %v", good, bad)
	}
}
