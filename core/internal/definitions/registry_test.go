package definitions

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
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

// git.mode none is the read-only reviewer (plan item 4): the vault spec says
// so, on both wires (Vaults and the pod profile share VaultSpec).
func TestVaultSpec_ReadOnly(t *testing.T) {
	spec := func(mode string) *Spec {
		return &Spec{Git: &Git{Mode: mode}, Credentials: &Credentials{Vault: mode, Services: []Service{{Host: "github.com", Paths: []string{"/o/r/*"}, Secret: "S"}}}}
	}
	r := &Registry{synced: true, lastGood: time.Now(), heads: map[string]Head{
		"none": {Name: "none", SHA: "s1", Effective: spec("none")},
		"push": {Name: "push", SHA: "s2", Effective: spec("push")},
	}}
	got := r.Vaults()
	i := slices.IndexFunc(got, func(v *agentfleetv1.VaultSpec) bool { return v.GetName() == "none" })
	j := slices.IndexFunc(got, func(v *agentfleetv1.VaultSpec) bool { return v.GetName() == "push" })
	if i < 0 || j < 0 || !got[i].GetReadOnly() || got[j].GetReadOnly() {
		t.Fatalf("Vaults = %v", got)
	}
	if v := VaultSpec(spec("none")); !v.GetReadOnly() || len(v.GetServices()) != 1 {
		t.Fatalf("VaultSpec = %v", v)
	}
}
