package agentsvc

import (
	"testing"
	"time"

	"github.com/MohammadBnei/agent-fleet/core/internal/auth"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
)

// docs/adr/0067 §11 without a database: who posts to a locked thread, and
// who may wake one idle past the window.
func TestLockAndWakeRules(t *testing.T) {
	member := func(origin string, groups ...string) Principal {
		return Principal{Member: &auth.Identity{Sub: "m", Email: "m@bnei.dev", Groups: groups}, Origin: origin}
	}
	owner := "m"
	shared := "shared"
	by := "root@bnei.dev"
	locked := &sessions.Session{Owner: &owner, Visibility: &shared, LockedBy: &by}
	island := "https://notes.bnei.dev"

	if canAnswer(member("", "shared-notes"), locked) {
		t.Error("a member, even the owner, posts to a locked thread")
	}
	if !canAnswer(member(island, auth.RequiredGroup), locked) {
		t.Error("a steward from an island is refused a locked thread")
	}
	if !canAnswer(member("", "shared-notes"), &sessions.Session{Owner: &owner, Visibility: &shared}) {
		t.Error("an unlocked shared thread refuses a member")
	}

	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	old := now.Add(-25 * time.Hour)
	recent := now.Add(-23 * time.Hour)
	live := "POD_PHASE_RUNNING"
	if !sessions.IsPodPhaseLive(&live) {
		t.Fatalf("%s is not live: pick a live phase", live)
	}
	for _, c := range []struct {
		name string
		p    Principal
		t    sessions.Session
		want bool
	}{
		{"member, idle 25h", member(island, "shared-notes"), sessions.Session{LastActiveAt: &old}, true},
		{"member, idle 23h", member(island, "shared-notes"), sessions.Session{LastActiveAt: &recent}, false},
		{"member, never active, created 25h ago", member("", "shared-notes"), sessions.Session{CreatedAt: old}, true},
		{"member, pod live", member(island, "shared-notes"), sessions.Session{LastActiveAt: &old, PodPhase: &live}, false},
		{"steward from an island", member(island, auth.RequiredGroup), sessions.Session{LastActiveAt: &old}, false},
		{"client", Principal{Client: "bot"}, sessions.Session{LastActiveAt: &old}, false},
	} {
		if got := needsWake(c.p, &c.t, now); got != c.want {
			t.Errorf("%s: needsWake = %v, want %v", c.name, got, c.want)
		}
	}
}
