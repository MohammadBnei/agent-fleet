package discord

import (
	"strings"
	"testing"
)

func TestThreadName_TruncatesLongDescription(t *testing.T) {
	long := strings.Repeat("a", 200)
	got := threadName("dream-analyst", long)

	if len(got) > 100 {
		t.Fatalf("thread name is %d runes, want <= 100 (Discord's limit): %q", len([]rune(got)), got)
	}
	if !strings.HasPrefix(got, "dream-analyst: aaa") {
		t.Fatalf("got %q, want it to start with repo prefix", got)
	}
}

func TestThreadName_KeepsShortDescriptionIntact(t *testing.T) {
	got := threadName("vos-monolith", "fix the login bug")
	want := "vos-monolith: fix the login bug"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
