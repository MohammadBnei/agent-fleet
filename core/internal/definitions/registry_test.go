package definitions

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Last-known-good is bounded: past staleAfter a create fails closed rather
// than pin a version that a narrowing core cannot fetch may have replaced.
func TestResolve_FailsClosedWhenStale(t *testing.T) {
	r := &Registry{synced: true, heads: map[string]Head{"a": {Name: "a", SHA: "s1"}}, lastGood: time.Now()}
	if _, err := r.Resolve("a"); err != nil {
		t.Fatalf("fresh: %v", err)
	}
	r.lastGood, r.lastErr = time.Now().Add(-staleAfter-time.Second), errors.New("provisioner down")
	if _, err := r.Resolve("a"); !errors.Is(err, errStale) {
		t.Fatalf("stale: want errStale, got %v", err)
	}
	heads, syncErrs := r.List()
	if len(heads) != 1 || !strings.Contains(strings.Join(syncErrs, "; "), "stale") {
		t.Fatalf("List keeps the heads and says they are stale: %v %v", heads, syncErrs)
	}
}

// EmbedOrigins: valid heads only, deduplicated, and nothing when stale.
func TestEmbedOrigins(t *testing.T) {
	embed := func(o ...string) *Spec { return &Spec{Embed: &Embed{Origins: o}} }
	r := &Registry{synced: true, lastGood: time.Now(), heads: map[string]Head{
		"a": {Name: "a", SHA: "s1", Effective: embed("https://opus.bnei.dev")},
		"b": {Name: "b", SHA: "s2", Effective: embed("https://opus.bnei.dev", "https://notes.bnei.dev")},
		"c": {Name: "c", Err: "bad"},
		"d": {Name: "d", SHA: "s4", Effective: &Spec{}},
	}}
	if got := r.EmbedOrigins(); strings.Join(got, ",") != "https://notes.bnei.dev,https://opus.bnei.dev" {
		t.Fatalf("EmbedOrigins = %v", got)
	}
	r.lastGood = time.Now().Add(-staleAfter - time.Second)
	if got := r.EmbedOrigins(); len(got) != 0 {
		t.Fatalf("stale: %v", got)
	}
}
