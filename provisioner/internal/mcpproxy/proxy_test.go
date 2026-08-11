package mcpproxy

import (
	"context"
	"testing"
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
