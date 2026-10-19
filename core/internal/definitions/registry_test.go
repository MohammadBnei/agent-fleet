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
