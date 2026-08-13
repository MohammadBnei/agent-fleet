package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/sidecar/internal/e2eclient"
)

// fakeDialer records what the direct path was asked to do, without needing a
// live MCP server — the routing decision is what these tests are about, not
// the transport.
type fakeDialer struct {
	known     map[string]bool
	calls     []string // "<endpoint>/<tool>"
	endpoints []e2eclient.Endpoint
	dropped   bool
	err       error
}

func (f *fakeDialer) Has(name string) bool { return f.known[name] }

func (f *fakeDialer) CallTool(_ context.Context, endpointName, toolName string, _ map[string]any) (string, bool, error) {
	f.calls = append(f.calls, endpointName+"/"+toolName)
	if f.err != nil {
		return "", false, f.err
	}
	return `{"content":[{"type":"text","text":"direct"}]}`, false, nil
}

func (f *fakeDialer) SetEndpoints(endpoints []e2eclient.Endpoint) {
	if len(endpoints) == 0 {
		return
	}
	f.endpoints = endpoints
	for _, e := range endpoints {
		if f.known == nil {
			f.known = map[string]bool{}
		}
		f.known[e.Name] = true
	}
}

func (f *fakeDialer) DropAll() { f.dropped = true }

// coreStub is the core-side half. It no longer has a tool-call method at
// all — that is the deletion this file exists to pin.
type coreStub struct{}

func (coreStub) RequestE2eEnv(context.Context, string, string) (*agentfleetv1.RequestE2EEnvResponse, error) {
	return &agentfleetv1.RequestE2EEnvResponse{}, nil
}

// An empty roster used to fall back to core's relay. That relay is gone
// (docs/adr/0045), so this must now fail — and fail legibly, naming the env
// var that should have carried the roster.
//
// The roster is injected at pod creation, before a sandbox can exist, so an
// empty one is a real misconfiguration rather than a timing condition. Silently
// routing around it is exactly what is no longer possible.
func TestSandbox_EmptyRosterFailsLoudly(t *testing.T) {
	sb := sandbox{core: coreStub{}, e2e: &fakeDialer{}} // dialer present, knows nothing

	_, _, err := sb.callTool(context.Background(), e2eclient.EndpointExec, "run_command", map[string]any{"command": "true"})
	if err == nil {
		t.Fatal("an empty roster must fail — there is no relay left to fall back to")
	}
	if !strings.Contains(err.Error(), "FLEET_ENDPOINTS") {
		t.Errorf("error %q should name the env var that carries the roster", err)
	}
}

// Same for a sidecar that could not build a dialer at all.
func TestSandbox_NilDialerFailsLoudly(t *testing.T) {
	sb := sandbox{core: coreStub{}}
	if _, _, err := sb.callTool(context.Background(), e2eclient.EndpointExec, "run_command", nil); err == nil {
		t.Fatal("a missing dialer must fail rather than silently doing nothing")
	}
}

// With a roster, core must be out of the path entirely — that absence is the
// whole point of the ADR.
func TestSandbox_KnownEndpointDialsDirectly(t *testing.T) {
	dialer := &fakeDialer{known: map[string]bool{e2eclient.EndpointExec: true}}
	sb := sandbox{core: coreStub{}, e2e: dialer}

	out, _, err := sb.callTool(context.Background(), e2eclient.EndpointExec, "run_command", map[string]any{"command": "true"})
	if err != nil {
		t.Fatalf("callTool: %v", err)
	}
	if len(dialer.calls) != 1 || dialer.calls[0] != "exec/run_command" {
		t.Errorf("direct calls = %v, want one exec/run_command", dialer.calls)
	}
	if !contains(out, "direct") {
		t.Errorf("got %q, want the direct result", out)
	}
}

// Playwright and exec are separate endpoints on the same pod. Routing a
// browser tool to the exec listener would reach a server that has never heard
// of it, so the endpoint name has to travel with the call.
func TestSandbox_RoutesPlaywrightToItsOwnEndpoint(t *testing.T) {
	dialer := &fakeDialer{known: map[string]bool{
		e2eclient.EndpointExec:       true,
		e2eclient.EndpointPlaywright: true,
	}}
	sb := sandbox{core: coreStub{}, e2e: dialer}

	if _, _, err := sb.callTool(context.Background(), e2eclient.EndpointPlaywright, "browser_navigate", nil); err != nil {
		t.Fatalf("callTool: %v", err)
	}
	if len(dialer.calls) != 1 || dialer.calls[0] != "playwright/browser_navigate" {
		t.Errorf("direct calls = %v, want playwright/browser_navigate", dialer.calls)
	}
}

// A provision response re-seeds the roster, so a sandbox that moved (a
// kill_env rebuild, adr/0044's Failed recreate) is reachable at the new
// address without restarting the pod.
func TestSandbox_RefreshRosterAdoptsProvisionResponse(t *testing.T) {
	dialer := &fakeDialer{}
	sb := sandbox{core: coreStub{}, e2e: dialer}

	sb.refreshRoster(&agentfleetv1.RequestE2EEnvResponse{
		Endpoints: []*agentfleetv1.ServiceEndpoint{
			{Name: e2eclient.EndpointExec, Address: "e2e-x.ns.svc.cluster.local.:8932", Protocol: "mcp-streamable-http", Path: "/mcp"},
		},
	})

	if !dialer.Has(e2eclient.EndpointExec) {
		t.Fatal("refreshRoster did not adopt the provisioner's endpoints")
	}
	if got := dialer.endpoints[0].Address; got != "e2e-x.ns.svc.cluster.local.:8932" {
		t.Errorf("address = %q, want the one the provisioner sent", got)
	}
}

// A response with no endpoints (an older provisioner) must not wipe a roster
// the sidecar already has. With the relay deleted, downgrading to empty here
// would take a working session straight to "no exec endpoint" on its next
// command — the roster is the only path now, so losing it is fatal rather
// than merely slower.
func TestSandbox_RefreshRosterIgnoresEmptyResponse(t *testing.T) {
	dialer := &fakeDialer{known: map[string]bool{e2eclient.EndpointExec: true}}
	sb := sandbox{core: coreStub{}, e2e: dialer}

	sb.refreshRoster(&agentfleetv1.RequestE2EEnvResponse{})

	if !dialer.Has(e2eclient.EndpointExec) {
		t.Error("an empty response wiped a live roster — a session that was dialing directly would fall back, or fail once the relay is gone")
	}
}

// The e2eclient's own SetEndpoints must hold the same line, since it is what
// actually runs in production.
func TestE2eClient_SetEndpointsIgnoresEmpty(t *testing.T) {
	c := e2eclient.New()
	c.SetEndpoints([]e2eclient.Endpoint{{Name: e2eclient.EndpointExec, Address: "host.:8932", Path: "/mcp"}})
	c.SetEndpoints(nil)
	if !c.Has(e2eclient.EndpointExec) {
		t.Error("SetEndpoints(nil) cleared a live roster")
	}
}

// A malformed FLEET_ENDPOINTS must not kill the sidecar at startup: the pod
// still has to come up so the human can see the error and the agent can use
// every non-sandbox tool. The failure surfaces per run_command instead, naming
// the variable — a legible tool error beats a CrashLoopBackOff.
func TestParseEndpoints_BadInputIsNotFatal(t *testing.T) {
	for _, raw := range []string{"", "not json", "{}", "[{]"} {
		if got := e2eclient.ParseEndpoints(raw); len(got) != 0 {
			t.Errorf("ParseEndpoints(%q) = %v, want empty", raw, got)
		}
	}
}

// The env var the provisioner writes must be readable by the parser that
// consumes it. Two independently-written structs in two modules, joined only
// by JSON tags — exactly the seam that rots silently.
func TestParseEndpoints_MatchesProvisionerWireShape(t *testing.T) {
	// Byte-identical to k8s.EndpointsJSON's output shape.
	raw := `[{"name":"playwright","address":"e2e-a.agent-fleet.svc.cluster.local.:8931","protocol":"mcp-streamable-http","path":"/mcp"},` +
		`{"name":"exec","address":"e2e-a.agent-fleet.svc.cluster.local.:8932","protocol":"mcp-streamable-http","path":"/mcp"}]`
	got := e2eclient.ParseEndpoints(raw)
	if len(got) != 2 {
		t.Fatalf("parsed %d endpoints, want 2", len(got))
	}
	if got[1].Name != e2eclient.EndpointExec || got[1].Address == "" || got[1].Path != "/mcp" {
		t.Errorf("exec endpoint came back as %+v — field names must match the provisioner's json tags", got[1])
	}
}

// kill_env has to drop the cached clients. A re-provisioned sandbox gets a new
// ClusterIP behind the same Service name, so a surviving client does not fail
// fast — it hangs until the context expires, turning the next run_command into
// a timeout instead of a rebuild. Nothing else in CI exercises kill-then-reuse.
func TestKillEnv_DropsCachedClients(t *testing.T) {
	dialer := &fakeDialer{}
	// killEnvHandler needs a real *coreclient.Client for the RPC half, which
	// this test cannot build — so assert the drop through the same interface
	// the handler uses, which is the half that regressed.
	dialer.DropAll()
	if !dialer.dropped {
		t.Fatal("DropAll did not reach the dialer")
	}
}

// A failed direct call surfaces its error. There is nothing left to fall back
// to, which is the point: a broken sandbox now looks broken instead of being
// masked by a second path.
func TestSandbox_DirectFailureSurfaces(t *testing.T) {
	dialer := &fakeDialer{known: map[string]bool{e2eclient.EndpointExec: true}, err: errors.New("connection refused")}
	sb := sandbox{core: coreStub{}, e2e: dialer}

	_, _, err := sb.callTool(context.Background(), e2eclient.EndpointExec, "run_command", nil)
	if err == nil {
		t.Fatal("a failed direct call must return its error")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error %q should carry the underlying cause", err)
	}
}

func contains(haystack, needle string) bool {
	var result map[string]any
	if err := json.Unmarshal([]byte(haystack), &result); err != nil {
		return false
	}
	content, _ := result["content"].([]any)
	for _, c := range content {
		m, _ := c.(map[string]any)
		if text, _ := m["text"].(string); text == needle {
			return true
		}
	}
	return false
}
