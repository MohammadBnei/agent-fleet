// Package e2edial lets core run one command in a task's sandbox by dialing it
// directly, instead of relaying through the provisioner (docs/adr/0045).
//
// core has exactly two reasons to touch a sandbox, both human-initiated from
// the dashboard: RestartE2EApp and GetE2EAppLog. Neither is the agent's work,
// which is why this lives in core rather than being routed through a worker.
//
// Deliberately one-shot, unlike the sidecar's e2eclient which caches.
// The difference is call frequency, not taste: the sidecar's run_command fires
// constantly through a session, while these fire when a human opens a drawer
// or clicks refresh (E2eManageDrawer loads the log once on open and otherwise
// only on a button — explicitly not on a timer). At that rate a cache buys
// nothing measurable and costs the stale-pod problem, with no natural point
// for core to evict on: it does not observe the sandbox lifecycle the way the
// sidecar observes its own kill_env.
package e2edial

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// EndpointExec is the roster entry serving run_command. Must match
// provisioner/internal/k8s.EndpointExec — the provisioner writes the name,
// this reads it.
const EndpointExec = "exec"

// Endpoint is the subset of the wire message this package needs.
type Endpoint struct {
	Name    string
	Address string
	Path    string
}

// Find picks one endpoint out of a roster. Returns ok=false for an empty
// roster, which is a provisioner too old to send one — the caller falls back
// to the relay rather than failing.
func Find(endpoints []Endpoint, name string) (Endpoint, bool) {
	for _, e := range endpoints {
		if e.Name == name {
			return e, true
		}
	}
	return Endpoint{}, false
}

// RunCommand dials the sandbox and runs one command, returning execmcp's
// result envelope as JSON — byte-identical to what the relay returned, so the
// caller's unwrapping is unchanged.
func RunCommand(ctx context.Context, ep Endpoint, command string) (resultJSON string, err error) {
	url := fmt.Sprintf("http://%s%s", ep.Address, ep.Path)
	c, err := client.NewStreamableHttpClient(url)
	if err != nil {
		return "", fmt.Errorf("build mcp client for %s: %w", url, err)
	}
	defer func() { _ = c.Close() }()

	if err := c.Start(ctx); err != nil {
		return "", fmt.Errorf("connect to sandbox at %s: %w", url, err)
	}
	if _, err := c.Initialize(ctx, mcp.InitializeRequest{}); err != nil {
		return "", fmt.Errorf("initialize sandbox mcp at %s: %w", url, err)
	}
	result, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "run_command",
			Arguments: map[string]any{"command": command},
		},
	})
	if err != nil {
		return "", fmt.Errorf("run_command in sandbox: %w", err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("marshal tool result: %w", err)
	}
	return string(encoded), nil
}
