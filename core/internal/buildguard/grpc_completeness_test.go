package buildguard

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestGrpcServersImplementEveryMethod guards the failure mode that has now
// cost this repo twice, and that no compiler will ever catch:
//
// protoc-gen-go-grpc emits an UnimplementedXServer struct, and every server
// embeds it so that adding an RPC doesn't break existing implementations.
// The cost of that forward-compatibility is that a MISSING implementation is
// also fine — it compiles, it links, it starts, and it fails at runtime with
// "Unimplemented desc = method Foo not implemented", reachable only by
// actually calling it.
//
// It has bitten in two different ways:
//
//   - protoc-gen-go-grpc uppercases initialisms, so a hand-written
//     `CallE2eTool` silently did not satisfy the generated `CallE2ETool`.
//   - docs/adr/0048 renamed GetTask -> GetSession and SaveSessionId ->
//     SaveAgentSessionId in the proto. Both Go handlers kept their old names.
//     Everything built; the first worker pod to start died on
//     "method GetSession not implemented".
//
// Note which services are NOT listed here, and why: DashboardService is
// served by connect-go, which generates no Unimplemented embed, and
// dashboard.Server carries an explicit
// `var _ agentfleetv1connect.DashboardServiceHandler = (*Server)(nil)`. The
// same mistake there is a compile error. This test exists to give the two
// gRPC services the assertion connect-go gets for free.
func TestGrpcServersImplementEveryMethod(t *testing.T) {
	root := repoRoot(t)

	cases := []struct {
		service   string
		generated string
		implDir   string
	}{
		{"CoreService", "proto/gen/go/agentfleet/v1/core_grpc.pb.go", "core/internal/coreserver"},
		{"ProvisionerService", "proto/gen/go/agentfleet/v1/provisioner_grpc.pb.go", "provisioner/internal/grpcserver"},
	}

	for _, c := range cases {
		t.Run(c.service, func(t *testing.T) {
			want := serverInterfaceMethods(t, filepath.Join(root, c.generated), c.service)
			if len(want) < 5 {
				t.Fatalf("%s: parsed only %d methods from %s — the generated layout changed and this guard is no longer guarding anything",
					c.service, len(want), c.generated)
			}
			have := implementedMethods(t, filepath.Join(root, c.implDir))

			var missing []string
			for _, m := range want {
				if !have[m] {
					missing = append(missing, m)
				}
			}
			if len(missing) > 0 {
				sort.Strings(missing)
				t.Errorf("%s: %d method(s) fall through to the Unimplemented stub: %s\n"+
					"    These compile fine and fail only when something calls them.\n"+
					"    Usually a proto rename that the Go handler did not follow, or\n"+
					"    protoc-gen-go-grpc's initialism casing (E2e -> E2E).",
					c.service, len(missing), strings.Join(missing, ", "))
			}
		})
	}
}

var (
	// The generated server interface block: `type XServer interface { ... }`.
	serverIfaceRe = regexp.MustCompile(`type (\w+Server) interface \{`)
	// A method line inside it: `Foo(context.Context, *Req) (*Resp, error)` or
	// the streaming form `Foo(*Req, grpc.ServerStream) error`.
	ifaceMethodRe = regexp.MustCompile(`^\s*([A-Z]\w*)\(`)
	implMethodRe  = regexp.MustCompile(`^func \(\w+ \*Server\) ([A-Z]\w*)\(`)
)

// serverInterfaceMethods reads the method names off the generated
// `<Service>Server` interface — the authoritative list of what must exist.
func serverInterfaceMethods(t *testing.T, path, service string) []string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v — this guard is worthless if it cannot find the generated code", path, err)
	}

	var out []string
	inTarget := false
	for _, line := range strings.Split(string(src), "\n") {
		if m := serverIfaceRe.FindStringSubmatch(line); m != nil {
			// Only the unimplemented-embedding interface, not the client one.
			inTarget = m[1] == service+"Server"
			continue
		}
		if !inTarget {
			continue
		}
		if strings.HasPrefix(line, "}") {
			break
		}
		if m := ifaceMethodRe.FindStringSubmatch(line); m != nil {
			// mustEmbedUnimplemented… is the compiler's own forward-compat
			// marker, satisfied by the embed rather than by a handler.
			if strings.HasPrefix(m[1], "MustEmbed") || strings.HasPrefix(m[1], "Unimplemented") {
				continue
			}
			out = append(out, m[1])
		}
	}
	return out
}

func implementedMethods(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	have := map[string]bool{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, line := range strings.Split(string(src), "\n") {
			if m := implMethodRe.FindStringSubmatch(line); m != nil {
				have[m[1]] = true
			}
		}
	}
	return have
}
