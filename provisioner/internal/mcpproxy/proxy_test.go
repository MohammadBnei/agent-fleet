package mcpproxy

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/client"
)

// TestProxiedTools_OmitsRunCommand is the load-bearing half of docs/adr/0039.
// run_command used to be prepended here so the sidecar would pick it up
// during its post-request_e2e_env registration pass. The sidecar now
// registers it statically at startup, with a handler that provisions a pod
// on demand — so listing it here again would have the sidecar overwrite that
// handler with a plain passthrough that can't, and only for sessions that
// happened to call request_e2e_env.
//
// No e2e pod exists in this test, which is exactly the pre-session state
// ProxiedTools is documented to tolerate: it returns nothing rather than
// erroring.
func TestProxiedTools_OmitsRunCommand(t *testing.T) {
	p := New(
		func(string) string { return "http://127.0.0.1:1/mcp" },
		func(string) string { return "http://127.0.0.1:1/mcp" },
	)
	for _, tool := range p.ProxiedTools(context.Background(), "task-1") {
		if tool.Name == RunCommandTool {
			t.Fatal("run_command must not be advertised here — the sidecar registers it statically")
		}
	}
}

// TestCallTool_RoutesRunCommandToExec pins the routing that survived the
// descriptor's removal: the name is still special-cased onto the exec
// listener's one-shot client rather than the cached Playwright one. Both
// URLs point at a closed port, so the assertion is which one was dialed.
func TestCallTool_RoutesRunCommandToExec(t *testing.T) {
	var playwrightDialed, execDialed bool
	p := New(
		func(string) string { playwrightDialed = true; return "http://127.0.0.1:1/mcp" },
		func(string) string { execDialed = true; return "http://127.0.0.1:1/mcp" },
	)
	//nolint:errcheck // the call cannot succeed against a closed port; the routing is what's under test
	_, _ = p.CallTool(context.Background(), "task-1", RunCommandTool, map[string]any{"command": "true"})

	if !execDialed {
		t.Error("run_command must route to the exec listener")
	}
	if playwrightDialed {
		t.Error("run_command must not go through the Playwright client")
	}
}

// TestCallTool_DropsClientOnError covers docs/adr/0044's stale-client fix.
// The cached client holds an MCP session bound to one pod, and four separate
// paths now replace a pod under a live task — kill_env, the terminating
// recreate, the Failed recreate, and the reconcile sweep. Only two of them
// ever called DropClient, so the other two left this map pointing at a
// corpse and every subsequent Playwright call failed until the session ended.
// Dropping on any call error covers all four in one place.
func TestCallTool_DropsClientOnError(t *testing.T) {
	p := New(
		func(string) string { return "http://127.0.0.1:1/mcp" },
		func(string) string { return "http://127.0.0.1:1/mcp" },
	)
	// Seed the cache the way a successful clientFor would, without needing a
	// live MCP server: the client is never Start()ed, so any call fails.
	stale, err := client.NewStreamableHttpClient("http://127.0.0.1:1/mcp")
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	p.clients["task-1"] = stale

	//nolint:errcheck // the failure is the point; what's under test is the cache afterwards
	_, _ = p.CallTool(context.Background(), "task-1", "browser_navigate", map[string]any{"url": "http://localhost"})

	if _, ok := p.clients["task-1"]; ok {
		t.Fatal("a client that failed a call must be dropped, so the next call rebuilds against the live pod")
	}
}
