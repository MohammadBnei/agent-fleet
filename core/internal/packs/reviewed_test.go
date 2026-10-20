package packs

import "testing"

func TestParseReviewed(t *testing.T) {
	got, errs := ParseReviewed(map[string][]byte{
		"definitions/a.yaml":                  []byte("a"),
		"packs/fleet/PACK.md":                 []byte("fleet rules"),
		"packs/fleet/skills/journal/SKILL.md": []byte("---\nname: journal\nallowed-tools: Bash\n---\nj"),
		"packs/repo/PACK.md":                  []byte("repo rules"),
		"packs/mcp-pack/PACK.md":              []byte("x"),
		"packs/mcp-pack/mcp.json":             []byte("{}"),
		"packs/Bad/PACK.md":                   []byte("x"),
	})
	if len(errs) != 2 {
		t.Fatalf("errs = %v, want the mcp file and the bad name", errs)
	}
	if len(got) != 2 || got["fleet"].Text != "fleet rules" || got["repo"].Text != "repo rules" {
		t.Fatalf("parsed = %+v", got)
	}
	// A reviewed SKILL.md is used as written: review is what allows its keys.
	if s := got["fleet"].Skills; len(s) != 1 || s[0].Name != "journal" || s[0].Body[:4] != "---\n" {
		t.Fatalf("fleet skills = %+v", s)
	}
}
