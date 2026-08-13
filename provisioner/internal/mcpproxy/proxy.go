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
	return p.cachedClient(ctx, taskID, p.urlForTask(taskID))
}

// execCacheKey namespaces the exec client in the same map as the Playwright
// one. A second map plus a second mutex would buy nothing — DropClient has to
// evict both together anyway, since both point at the same pod.
func execCacheKey(taskID string) string { return taskID + "|exec" }

// cachedClient builds at most one live MCP client per key.
//
// The dial + handshake deliberately happens OUTSIDE the mutex. It used to run
// under it, on this one fleet-wide Proxy, which meant that while any single
// task was connecting — up to the context deadline, against a sandbox that may
// not be listening yet — every other task's CallTool and ProxiedTools blocked
// behind the same lock. One wedged sandbox stalled tool calls fleet-wide. That
// is a far better explanation of "tool calls feel slow" than the 0-1ms of gRPC
// relay docs/adr/0045 measured.
//
// The cost of dialing outside the lock is that two callers can race and both
// build a client. That is fine and cheap: the loser is closed, the winner is
// shared. Losing a redundant handshake beats serializing every task.
func (p *Proxy) cachedClient(ctx context.Context, key, url string) (*client.Client, error) {
	p.mu.Lock()
	c, ok := p.clients[key]
	p.mu.Unlock()
	if ok {
		return c, nil
	}

	fresh, err := client.NewStreamableHttpClient(url)
	if err != nil {
		return nil, err
	}
	if err := fresh.Start(ctx); err != nil {
		return nil, err
	}
	if _, err := fresh.Initialize(ctx, mcp.InitializeRequest{}); err != nil {
		return nil, err
	}

	p.mu.Lock()
	winner, lost := p.clients[key]
	if !lost {
		p.clients[key] = fresh
	}
	p.mu.Unlock()

	if lost {
		// Closed outside the lock — Close does I/O, and holding the mutex
		// across I/O is the whole bug this function exists to avoid.
		_ = fresh.Close()
		return winner, nil
	}
	return fresh, nil
}

// DropClient mirrors dropClient — called on teardown so a stale client
// isn't reused against a pod that no longer exists.
//
// Both of the task's clients go: Playwright and exec point at the same pod, so
// any event that invalidates one invalidates the other. Dropping only the
// Playwright key would leave the exec client holding a session bound to a
// corpse, which is the exact failure DropClient exists to prevent.
func (p *Proxy) DropClient(taskID string) {
	p.mu.Lock()
	stale := make([]*client.Client, 0, 2)
	for _, key := range []string{taskID, execCacheKey(taskID)} {
		if c, ok := p.clients[key]; ok {
			stale = append(stale, c)
			delete(p.clients, key)
		}
	}
	p.mu.Unlock()

	for _, c := range stale {
		if err := c.Close(); err != nil {
			slog.Error("failed closing e2e mcp client", "taskId", taskID, "error", err)
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

// callExec caches its client, like the Playwright path does.
//
// It used to build a one-shot client per call, reasoning that run_command has
// a fixed tool set with nothing to discover. That is true of *discovery* and
// false of *cost*: it paid a dial + Start + Initialize on every single
// run_command, which is the fleet's most-used tool. Nothing about having one
// fixed tool makes a fresh TCP connection and MCP handshake free.
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
	c, err := p.cachedClient(ctx, execCacheKey(taskID), p.execURLForTask(taskID))
	if err != nil {
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
	if err != nil {
		// Same reasoning as the Playwright path above: a cached client
		// outlives the pod it was bound to, and every replace-the-pod path
		// would otherwise leave it pointing at a corpse. Caching without this
		// would trade one handshake per call for a wedged session per pod
		// replacement — a strictly worse deal.
		p.DropClient(taskID)
		return nil, err
	}
	return result, nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
