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

type Proxy struct {
	mu         sync.Mutex
	clients    map[string]*client.Client
	urlForTask func(taskID string) string
}

func New(urlForTask func(taskID string) string) *Proxy {
	return &Proxy{clients: make(map[string]*client.Client), urlForTask: urlForTask}
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
	c, err := p.clientFor(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: name, Arguments: args},
	})
}
