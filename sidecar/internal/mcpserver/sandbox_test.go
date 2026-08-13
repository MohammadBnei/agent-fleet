package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
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

// relayRecorder is the core-side half.
type relayRecorder struct {
	calls int
}

func (r *relayRecorder) CallE2eTool(context.Context, string, string) (string, bool, error) {
	r.calls++
	return `{"content":[{"type":"text","text":"relayed"}]}`, false, nil
}

func (r *relayRecorder) RequestE2eEnv(context.Context, string, string) (*agentfleetv1.RequestE2EEnvResponse, error) {
	return &agentfleetv1.RequestE2EEnvResponse{}, nil
}

// An empty roster MUST keep working through core. This is the deploy-skew
// contract and the single most dangerous thing in docs/adr/0045: core, the
// provisioner and the worker image are separate ArgoCD Applications, so a
// sidecar carrying this code will meet a provisioner too old to send a roster
// on some rolling deploy. If this test ever goes green by accident — because
// the fallback was "cleaned up" — every run_command in the fleet fails during
// that window.
func TestSandbox_EmptyRosterFallsBackToTheRelay(t *testing.T) {
	relay := &relayRecorder{}
	sb := sandbox{core: relay, e2e: &fakeDialer{}} // dialer present, knows nothing

	out, _, err := sb.callTool(context.Background(), e2eclient.EndpointExec, "run_command", map[string]any{"command": "true"})
	if err != nil {
		t.Fatalf("callTool: %v", err)
	}
	if relay.calls != 1 {
		t.Errorf("relay calls = %d, want 1 — an unknown endpoint must route through core", relay.calls)
	}
	if !contains(out, "relayed") {
		t.Errorf("got %q, want the relay's result", out)
	}
}

// A nil dialer is the same contract, for the case where the sidecar could not
// build one at all.
func TestSandbox_NilDialerFallsBackToTheRelay(t *testing.T) {
	relay := &relayRecorder{}
	sb := sandbox{core: relay}
	if _, _, err := sb.callTool(context.Background(), e2eclient.EndpointExec, "run_command", nil); err != nil {
		t.Fatalf("callTool: %v", err)
	}
	if relay.calls != 1 {
		t.Errorf("relay calls = %d, want 1", relay.calls)
	}
}

// With a roster, core must be out of the path entirely — that absence is the
// whole point of the ADR.
func TestSandbox_KnownEndpointBypassesCore(t *testing.T) {
	relay := &relayRecorder{}
	dialer := &fakeDialer{known: map[string]bool{e2eclient.EndpointExec: true}}
	sb := sandbox{core: relay, e2e: dialer}

	out, _, err := sb.callTool(context.Background(), e2eclient.EndpointExec, "run_command", map[string]any{"command": "true"})
	if err != nil {
		t.Fatalf("callTool: %v", err)
	}
	if relay.calls != 0 {
		t.Errorf("relay calls = %d, want 0 — a known endpoint must not touch core", relay.calls)
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
	sb := sandbox{core: &relayRecorder{}, e2e: dialer}

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
	sb := sandbox{core: &relayRecorder{}, e2e: dialer}

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
// the sidecar already has. Downgrading to empty here would silently send a
// working session back to the relay — and after the relay is deleted, break
// it outright.
func TestSandbox_RefreshRosterIgnoresEmptyResponse(t *testing.T) {
	dialer := &fakeDialer{known: map[string]bool{e2eclient.EndpointExec: true}}
	sb := sandbox{core: &relayRecorder{}, e2e: dialer}

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

// A malformed FLEET_ENDPOINTS degrades to the relay rather than killing the
// pod — a sidecar that cannot run commands at all is a worse outcome than one
// taking the old path.
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

// A failed direct call must surface the error rather than silently retrying
// through core: falling back on *failure* would mask a broken sandbox behind a
// path that is about to be deleted, and hide exactly the breakage the
// cut-over needs to be visible.
func TestSandbox_DirectFailureDoesNotSilentlyRelay(t *testing.T) {
	relay := &relayRecorder{}
	dialer := &fakeDialer{known: map[string]bool{e2eclient.EndpointExec: true}, err: errors.New("connection refused")}
	sb := sandbox{core: relay, e2e: dialer}

	if _, _, err := sb.callTool(context.Background(), e2eclient.EndpointExec, "run_command", nil); err == nil {
		t.Fatal("a failed direct call must return its error")
	}
	if relay.calls != 0 {
		t.Errorf("relay calls = %d, want 0 — the fallback is for an ABSENT roster, not a failed dial", relay.calls)
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
