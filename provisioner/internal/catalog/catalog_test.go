package catalog

import "testing"

// These lists must match db/migrations/000003_repo_profiles.up.sql's
// tool_key/service_key CHECK constraints exactly — this test is the
// guard against the two enumerations silently drifting apart (core has
// no shared package with the provisioner to enforce this at compile time,
// docs/adr/0034's judgment call #3).
var dbCheckToolKeys = []string{"go-toolchain", "bun-toolchain", "golangci-lint", "buf"}
var dbCheckServiceKeys = []string{"postgres", "redis"}

func TestTools_MatchesDBCheckConstraint(t *testing.T) {
	if len(Tools) != len(dbCheckToolKeys) {
		t.Fatalf("catalog.Tools has %d entries, db CHECK constraint allows %d", len(Tools), len(dbCheckToolKeys))
	}
	for _, key := range dbCheckToolKeys {
		if !KnownToolKey(key) {
			t.Errorf("db-allowed tool_key %q has no catalog.Tools entry", key)
		}
		def := Tools[key]
		if def.CopyImage == "" {
			t.Errorf("tool %q: CopyImage is empty", key)
		}
		if len(def.CopyCmd) == 0 {
			t.Errorf("tool %q: CopyCmd is empty", key)
		}
	}
}

func TestServices_MatchesDBCheckConstraint(t *testing.T) {
	if len(Services) != len(dbCheckServiceKeys) {
		t.Fatalf("catalog.Services has %d entries, db CHECK constraint allows %d", len(Services), len(dbCheckServiceKeys))
	}
	for _, key := range dbCheckServiceKeys {
		if !KnownServiceKey(key) {
			t.Errorf("db-allowed service_key %q has no catalog.Services entry", key)
		}
		if Services[key].Port == 0 {
			t.Errorf("service %q: Port is unset", key)
		}
	}
}

func TestKnownToolKey_UnknownKey(t *testing.T) {
	if KnownToolKey("not-a-real-tool") {
		t.Error("KnownToolKey(unknown) = true, want false")
	}
}

func TestKnownServiceKey_UnknownKey(t *testing.T) {
	if KnownServiceKey("not-a-real-service") {
		t.Error("KnownServiceKey(unknown) = true, want false")
	}
}
