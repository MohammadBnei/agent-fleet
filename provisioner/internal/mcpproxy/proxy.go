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

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// runCommandTool is the one statically-known tool served by the e2e pod's
// first-party execmcp listener — unlike Playwright's tool set (discovered
// at runtime, hence the client cache below), there's nothing to discover
// here, so it doesn't go through the same cached-client/ListTools path.
var runCommandTool = mcp.Tool{
	Name:        "run_command",
	Description: "Run a shell command in the e2e pod's worktree (bash -lc), for anything the app needs beyond its initial start: installing a missed dependency, restarting the dev server, running the app's own tests, inspecting a failure. Returns stdout, stderr, and exit code — a nonzero exit is not an error, it's information. A long-running process (e.g. a dev server) should background itself (trailing &) rather than block this call; timeout is 5 minutes.",
	InputSchema: mcp.ToolInputSchema{
		Type:     "object",
		Required: []string{"command"},
		Properties: map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "Shell command, run via bash -lc from the worktree root",
			},
		},
	},
}

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
func (p *Proxy) ProxiedTools(ctx context.Context, taskID string) []mcp.Tool {
	tools := []mcp.Tool{runCommandTool}
	c, err := p.clientFor(ctx, taskID)
	if err != nil {
		slog.Info("no playwright mcp tools available yet", "taskId", taskID, "error", err)
		return tools
	}
	result, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		slog.Info("no playwright mcp tools available yet", "taskId", taskID, "error", err)
		return tools
	}
	return append(tools, result.Tools...)
}

func (p *Proxy) CallTool(ctx context.Context, taskID, name string, args map[string]any) (*mcp.CallToolResult, error) {
	if name == runCommandTool.Name {
		return p.callExec(ctx, taskID, args)
	}
	c, err := p.clientFor(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: name, Arguments: args},
	})
}

// callExec is a one-shot client, not a cached one — run_command is a
// single fixed tool with nothing to discover, so there's no lifecycle to
// manage the way the Playwright client's ListTools-then-repeated-CallTool
// pattern needs.
func (p *Proxy) callExec(ctx context.Context, taskID string, args map[string]any) (*mcp.CallToolResult, error) {
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
	return c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: runCommandTool.Name, Arguments: args},
	})
}
