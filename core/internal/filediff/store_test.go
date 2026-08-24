package filediff

import "testing"

// The whole exchange in one test, because every step of it is a place the
// console can end up polling forever if the state machine leaks.
func TestWantThenAnswerThenRead(t *testing.T) {
	s := New()

	if !s.Want("sess", "src/foo.ts") {
		t.Fatal("first want should be accepted")
	}
	if _, ok := s.Take("sess", "src/foo.ts"); ok {
		t.Error("nothing has answered yet")
	}

	// Draining is what the sidecar's next tick does.
	if got := s.Wanted("sess"); len(got) != 1 || got[0] != "src/foo.ts" {
		t.Fatalf("expected the wanted path, got %v", got)
	}
	// Cleared on read: a path the pod cannot answer must not be re-asked on
	// every tick forever.
	if got := s.Wanted("sess"); got != nil {
		t.Errorf("wanted paths should clear on read, got %v", got)
	}

	s.Put("sess", map[string]string{"src/foo.ts": "@@ -1 +1 @@"})
	diff, ok := s.Take("sess", "src/foo.ts")
	if !ok || diff != "@@ -1 +1 @@" {
		t.Fatalf("expected the diff back, got %q (present=%v)", diff, ok)
	}
	// Read once. The PVC survives teardown, so a re-warmed session has the
	// same paths with different contents; a cache with no clock would serve
	// the old one indefinitely.
	if _, ok := s.Take("sess", "src/foo.ts"); ok {
		t.Error("an answer must be readable exactly once")
	}
}

// An empty diff is git saying "nothing here" — committed, reverted, untracked.
// It has to survive the round trip as a real answer, because the alternative is
// indistinguishable from "still waiting".
func TestEmptyDiffIsAnAnswer(t *testing.T) {
	s := New()
	s.Put("sess", map[string]string{"gone.txt": ""})
	diff, ok := s.Take("sess", "gone.txt")
	if !ok {
		t.Fatal("an empty diff must still be present")
	}
	if diff != "" {
		t.Errorf("expected empty, got %q", diff)
	}
}

func TestWantIsRefusedAtTheCap(t *testing.T) {
	s := New()
	for i := range maxWantedPerSession {
		if !s.Want("sess", string(rune('a'+i))+".txt") {
			t.Fatalf("want %d should fit under the cap", i)
		}
	}
	// Refused, not silently dropped: a dropped want reads exactly like a slow
	// pod, and the console polls forever.
	if s.Want("sess", "one-too-many.txt") {
		t.Error("expected the cap to refuse")
	}
	// A path already wanted is not a new want and must still be accepted.
	if !s.Want("sess", "a.txt") {
		t.Error("re-wanting a pending path should be accepted")
	}
}

func TestPutTruncatesAnOversizedDiff(t *testing.T) {
	s := New()
	huge := make([]byte, MaxDiffBytes+100)
	for i := range huge {
		huge[i] = 'x'
	}
	s.Put("sess", map[string]string{"big.lock": string(huge)})
	diff, _ := s.Take("sess", "big.lock")
	if len(diff) <= MaxDiffBytes {
		t.Fatalf("expected a truncation marker past the cap, got %d bytes", len(diff))
	}
	if len(diff) > MaxDiffBytes+64 {
		t.Errorf("truncated diff is still %d bytes", len(diff))
	}
}

// The leak shape this type has: a map keyed by session id, fed by pods, with no
// lifecycle event telling it a session ended.
func TestSessionsAreBounded(t *testing.T) {
	s := New()
	for i := range maxSessions + 10 {
		id := string(rune('A'+i%26)) + string(rune('0'+i/26))
		s.Want(id, "x.txt")
		s.Put(id, map[string]string{"x.txt": "d"})
	}
	if len(s.wanted) > maxSessions {
		t.Errorf("wanted map grew to %d sessions", len(s.wanted))
	}
	if len(s.ready) > maxSessions {
		t.Errorf("ready map grew to %d sessions", len(s.ready))
	}
}
