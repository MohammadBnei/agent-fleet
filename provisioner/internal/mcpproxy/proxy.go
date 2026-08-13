// Package mcpproxy is the Go port of mcpProxy.ts: proxies Playwright MCP
// tool calls to whichever e2e pod is currently live for a task. The
// worker's Agent SDK session never connects to the ephemeral pod
// directly — it always talks to e2e-provisioner's stable /mcp/:taskId
// endpoint, which forwards here.
package mcpproxy

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// RunCommandTool names the one statically-known tool served by the e2e
// pod's first-party execmcp listener — unlike Playwright's tool set
// (discovered at runtime, hence the client cache below), there's nothing to
// discover here, so it doesn't go through the same cached-client/ListTools
// path.
//
// Only the name lives here. The agent-facing descriptor is registered
// statically by the sidecar (docs/adr/0039) rather than surfaced through
// ProxiedTools, so a copy of the description here would be a third one
// nobody reads — the two that remain are the sidecar's (what the agent
// sees) and execmcp's own (what the server advertises).
const RunCommandTool = "run_command"

type Proxy struct {
	mu             sync.Mutex
	clients        map[string]*client.Client
	urlForTask     func(taskID string) string
	execURLForTask func(taskID string) string
}

func New(urlForTask, execURLForTask func(taskID string) string) *Proxy {
	return &Proxy{clients: make(map[string]*client.Client), urlForTask: urlForTask, execURLForTask: execURLForTask}
}

func (p *Proxy) clientFor(ctx context.Context, taskID string) (*client.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if c, ok := p.clients[taskID]; ok {
		return c, nil
	}
	c, err := client.NewStreamableHttpClient(p.urlForTask(taskID))
	if err != nil {
		return nil, err
	}
	if err := c.Start(ctx); err != nil {
		return nil, err
	}
	if _, err := c.Initialize(ctx, mcp.InitializeRequest{}); err != nil {
		return nil, err
	}
	p.clients[taskID] = c
	return c, nil
}

// DropClient mirrors dropClient — called on teardown so a stale client
// isn't reused against a pod that no longer exists.
func (p *Proxy) DropClient(taskID string) {
	p.mu.Lock()
	c, ok := p.clients[taskID]
	delete(p.clients, taskID)
	p.mu.Unlock()
	if ok {
		if err := c.Close(); err != nil {
			slog.Error("failed closing playwright mcp client", "taskId", taskID, "error", err)
		}
	}
}

// ProxiedTools mirrors proxiedTools: returns [] rather than erroring when
// no e2e pod is live yet — that's the normal pre-request_e2e_env state.
//
// run_command is deliberately NOT listed here. The sidecar registers it
// statically at startup so it exists from the session's first turn, with a
// handler that provisions a pod on demand (docs/adr/0039); re-listing it
// here would make the sidecar's dynamic-registration pass overwrite that
// handler with a plain passthrough that can't auto-provision.
func (p *Proxy) ProxiedTools(ctx context.Context, taskID string) []mcp.Tool {
	c, err := p.clientFor(ctx, taskID)
	if err != nil {
		slog.Info("no playwright mcp tools available yet", "taskId", taskID, "error", err)
		return nil
	}
	result, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		slog.Info("no playwright mcp tools available yet", "taskId", taskID, "error", err)
		return nil
	}
	return result.Tools
}

func (p *Proxy) CallTool(ctx context.Context, taskID, name string, args map[string]any) (*mcp.CallToolResult, error) {
	if name == RunCommandTool {
		return p.callExec(ctx, taskID, args)
	}
	c, err := p.clientFor(ctx, taskID)
	if err != nil {
		return nil, err
	}
	result, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: name, Arguments: args},
	})
	if err != nil {
		// The cached client holds an MCP session bound to one pod. Every path
		// that replaces a pod under a live task — kill_env, the terminating
		// recreate, docs/adr/0044's Failed recreate, the reconcile sweep —
		// otherwise leaves this client pointing at a corpse, and only
		// kill_env/tearDownE2e ever called DropClient. Dropping on any call
		// error covers all of them in one place: the next call rebuilds
		// against whatever pod is live now.
		slog.Info("dropping playwright mcp client after a failed call", "taskId", taskID, "tool", name, "error", err)
		p.DropClient(taskID)
		return nil, err
	}
	return result, nil
}

// callExec is a one-shot client, not a cached one — run_command is a
// single fixed tool with nothing to discover, so there's no lifecycle to
// manage the way the Playwright client's ListTools-then-repeated-CallTool
// pattern needs.
//
// It logs `init_ms` (dial + MCP handshake) separately from `call_ms` (the
// command's own runtime) because only the first is overhead this fleet can
// remove — the two hops around it were measured at 0-1ms (docs/adr/0045), so
// if a run_command "feels slow" the answer is in one of these two numbers,
// not in the topology. Read with:
//
//	{namespace="agent-fleet", service_name="provisioner"} | json
//	  |= "mcp exec call" | unwrap init_ms
//
// docs/adr/0045 pins this field shape: whatever ends up owning the dial must
// emit the same two names, or the before/after comparison across the
// cut-over silently stops being a comparison.
func (p *Proxy) callExec(ctx context.Context, taskID string, args map[string]any) (*mcp.CallToolResult, error) {
	initStart := time.Now()
	c, err := client.NewStreamableHttpClient(p.execURLForTask(taskID))
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	if err := c.Start(ctx); err != nil {
		return nil, err
	}
	if _, err := c.Initialize(ctx, mcp.InitializeRequest{}); err != nil {
		return nil, err
	}
	initMS := time.Since(initStart).Milliseconds()

	callStart := time.Now()
	result, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: RunCommandTool, Arguments: args},
	})
	slog.Info("mcp exec call",
		"taskId", taskID,
		"init_ms", initMS,
		"call_ms", time.Since(callStart).Milliseconds(),
		"error", errString(err))
	return result, err
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
