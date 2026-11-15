package decisions_test

import (
	"testing"

	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
)

// NonBlocking's key is the match key, not the full input: a Bash retry with a
// reworded description dedups to the same open request (ADR-0070 §6).
func TestNonBlocking_KeyIgnoresDescription(t *testing.T) {
	ask := func(input string) string {
		_, key, _, ok := decisions.NonBlocking(`{"nb":1,"tool":"Bash","input":` + input + `}`)
		if !ok {
			t.Fatal("not a non-blocking request")
		}
		return key
	}
	a := ask(`{"command":"ls","description":"list"}`)
	if b := ask(`{"command":"ls","description":"list files"}`); a != b || a == "" {
		t.Fatalf("reworded description changed the key: %q vs %q", a, b)
	}
	if c := ask(`{"command":"rm x","description":"list"}`); c == a {
		t.Fatal("a different command shares the key")
	}
}
