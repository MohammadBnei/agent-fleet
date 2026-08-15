package catalog

import "testing"

// These used to be checked against db/migrations/000003_repo_profiles.up.sql's
// tool_key/service_key CHECK constraints. That migration, that table and those
// constraints are all gone — repo_profiles was deleted in docs/adr/0048 and the
// schema squashed into 000001_init. The old test asserted len(Tools) == 5
// against a constraint that no longer existed, and passed only because both
// numbers happened to still be 5.
//
// What is worth guarding is the shape: every tool key has to be stageable, and
// every service key has to name the env var its client library actually reads.
var wantToolKeys = []string{"cluster-access"}
var wantServiceKeys = []string{"postgres", "redis"}

func TestTools_EveryKeyIsStageable(t *testing.T) {
	if len(Tools) != len(wantToolKeys) {
		t.Fatalf("catalog.Tools has %d entries, want %d (%v)", len(Tools), len(wantToolKeys), wantToolKeys)
	}
	for _, key := range wantToolKeys {
		if !KnownToolKey(key) {
			t.Errorf("tool_key %q has no catalog.Tools entry", key)
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

// TestTools_CarriesNoToolchain is the guard for docs/adr/0048 §6: a repo's
// toolchain is repos.image, never an init container. Re-adding one here would
// also re-add the PATH shadowing that made go-toolchain silently override the
// worker image's own Go.
func TestTools_CarriesNoToolchain(t *testing.T) {
	for _, key := range []string{"go-toolchain", "bun-toolchain", "golangci-lint", "buf"} {
		if KnownToolKey(key) {
			t.Errorf("tool_key %q is back — a toolchain belongs in repos.image (docs/adr/0048 §6)", key)
		}
	}
}

func TestServices_EveryKeyIsResolvable(t *testing.T) {
	if len(Services) != len(wantServiceKeys) {
		t.Fatalf("catalog.Services has %d entries, want %d (%v)", len(Services), len(wantServiceKeys), wantServiceKeys)
	}
	for _, key := range wantServiceKeys {
		if !KnownServiceKey(key) {
			t.Errorf("service_key %q has no catalog.Services entry", key)
		}
		if Services[key].Port == 0 {
			t.Errorf("service %q: Port is unset", key)
		}
		if Services[key].EnvVarName == "" {
			t.Errorf("service %q: EnvVarName is unset", key)
		}
	}
}

// TestServices_EnvVarNamesMatchAppConventions is a regression test for a
// bug caught live via /kind-local: the env var names this catalog injects
// must be what the actual consuming app looks for (Prisma's DATABASE_URL,
// ioredis's REDIS_URL), not an invented SERVICE_<KEY>_URL scheme —
// dream-analyst's own Prisma config errored with "Cannot resolve
// environment variable: DATABASE_URL" until this matched.
func TestServices_EnvVarNamesMatchAppConventions(t *testing.T) {
	want := map[string]string{
		"postgres": "DATABASE_URL",
		"redis":    "REDIS_URL",
	}
	for key, envVar := range want {
		if got := Services[key].EnvVarName; got != envVar {
			t.Errorf("Services[%q].EnvVarName = %q, want %q", key, got, envVar)
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
