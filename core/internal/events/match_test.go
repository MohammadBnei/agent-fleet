package events

import "testing"

func TestGlob(t *testing.T) {
	for _, c := range []struct {
		g, s string
		want bool
	}{
		{"github/MohammadBnei/*", "github/MohammadBnei/notes/pr/4", true},
		{"github/*/pr/?", "github/o/r/pr/4", true},
		{"github/*/pr/?", "github/o/r/pr/42", false},
		{"*", "", true},
		{"", "", true},
		{"", "a", false},
		{"a*b*c", "aXbYbZc", true},
		{"a*b*c", "aXbYbZ", false},
		{"alert/*", "github/x", false},
	} {
		if got := Glob(c.g, c.s); got != c.want {
			t.Errorf("Glob(%q, %q) = %v, want %v", c.g, c.s, got, c.want)
		}
	}
	if !SubjectMatches(nil, "x") || !TypeMatches(nil, "x") || TypeMatches([]string{"dev.bnei.github."}, "dev.bnei.alert.firing") {
		t.Error("empty filters match everything; a prefix only its own")
	}
}
