// Package e2eclient dials this task's sandbox directly, instead of relaying
// tool calls through core (docs/adr/0045).
//
// It exists because the relay cost a proto pair and a handler per service and
// put a shell command in the path of the fleet's Postgres-credential holder —
// not because it was slow. The two gRPC hops it removes were measured at
// ~1ms of a 751ms p50 call. Nothing here should be justified on speed.
//
// Scope: one task. A sidecar only ever serves its own pod's session, so there
// is no map keyed by task and no fleet-wide lock — the cross-task serializer
// that made mcpproxy's equivalent a problem cannot exist here by
// construction.
package e2eclient

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// Endpoint mirrors the wire/env shape the provisioner produces. Field names
// match both delivery channels — FLEET_ENDPOINTS at pod creation and
// RequestE2eEnvResponse.endpoints on every provision — so one parser covers
// both.
type Endpoint struct {
	Name     string `json:"name"`
	Address  string `json:"address"`
	Protocol string `json:"protocol"`
	Path     string `json:"path"`
	Token    string `json:"token"`
}

// Endpoint names the sandbox serves.
const (
	EndpointExec       = "exec"
	EndpointPlaywright = "playwright"
)

type Client struct {
	mu        sync.Mutex
	endpoints map[string]Endpoint
	clients   map[string]*client.Client
}

func New() *Client {
	return &Client{endpoints: map[string]Endpoint{}, clients: map[string]*client.Client{}}
}

// ParseEndpoints reads the FLEET_ENDPOINTS env var. A malformed or absent
// value is not fatal: it leaves the roster empty, which the caller treats as
// "fall back to the relay" rather than "fail". A bad env var degrading to the
// old path beats a worker pod that cannot run commands at all.
func ParseEndpoints(raw string) []Endpoint {
	if raw == "" {
		return nil
	}
	var out []Endpoint
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		slog.Error("e2eclient: FLEET_ENDPOINTS is not valid JSON, falling back to the relay", "error", err)
		return nil
	}
	return out
}

// SetEndpoints replaces the roster. Called with the env var at startup and
// again with every RequestE2eEnv response, so an address that changes under a
// running pod (a namespace move, a port change) reaches the sidecar without a
// restart.
//
// Replacing an endpoint drops its cached client: the old one holds a session
// bound to the address it was built for.
func (c *Client) SetEndpoints(endpoints []Endpoint) {
	if len(endpoints) == 0 {
		return // never downgrade a live roster to empty — see Has()
	}
	var stale []*client.Client
	c.mu.Lock()
	for _, e := range endpoints {
		if prev, ok := c.endpoints[e.Name]; ok && prev.Address != e.Address {
			if cl, ok := c.clients[e.Name]; ok {
				stale = append(stale, cl)
				delete(c.clients, e.Name)
			}
		}
		c.endpoints[e.Name] = e
	}
	c.mu.Unlock()
	closeAll(stale)
}

// Has reports whether the roster knows this endpoint. The caller uses it to
// decide between direct dial and the relay, which is the whole deploy-skew
// contract: a new sidecar against an old provisioner sees an empty roster and
// keeps working through core.
func (c *Client) Has(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.endpoints[name]
	return ok
}

// CallTool runs one tool on the named endpoint.
//
// Returns the result JSON in the same envelope the relay produced, so callers
// that already parse a CallToolResult do not care which path it came from.
func (c *Client) CallTool(ctx context.Context, endpointName, toolName string, args map[string]any) (resultJSON string, isError bool, err error) {
	return c.callToolWithRetry(ctx, endpointName, toolName, args, false)
}

// ponytail: single retry recovers from stale cached client
func (c *Client) callToolWithRetry(ctx context.Context, endpointName, toolName string, args map[string]any, isRetry bool) (resultJSON string, isError bool, err error) {
	cl, err := c.clientFor(ctx, endpointName)
	if err != nil {
		return "", false, err
	}
	result, err := cl.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: toolName, Arguments: args},
	})
	if err != nil {
		// A cached client outlives the pod it was bound to, and every path
		// that replaces a sandbox under a live task — kill_env, adr/0044's
		// Failed recreate, the terminating recreate, the reconcile sweep —
		// would otherwise leave it pointing at a corpse. Dropping on any call
		// error covers all of them in one place, the same fix adr/0044 made
		// on the provisioner side.
		slog.Info("e2eclient: dropping client after a failed call", "endpoint", endpointName, "tool", toolName, "error", err, "retry", isRetry)
		c.Drop(endpointName)
		if !isRetry {
			return c.callToolWithRetry(ctx, endpointName, toolName, args, true)
		}
		return "", false, err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return "", false, fmt.Errorf("marshal tool result: %w", err)
	}
	return string(encoded), result.IsError, nil
}

// ListTools is the diagnostic half — what the live pod actually serves, as
// opposed to the static snapshot the sidecar registers (docs/adr/0044).
func (c *Client) ListTools(ctx context.Context, endpointName string) ([]mcp.Tool, error) {
	return c.listToolsWithRetry(ctx, endpointName, false)
}

func (c *Client) listToolsWithRetry(ctx context.Context, endpointName string, isRetry bool) ([]mcp.Tool, error) {
	cl, err := c.clientFor(ctx, endpointName)
	if err != nil {
		return nil, err
	}
	result, err := cl.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		slog.Info("e2eclient: dropping client after failed list", "endpoint", endpointName, "error", err, "retry", isRetry)
		c.Drop(endpointName)
		if !isRetry {
			return c.listToolsWithRetry(ctx, endpointName, true)
		}
		return nil, err
	}
	return result.Tools, nil
}

// Drop closes and forgets one endpoint's client. DropAll is what kill_env
// needs: a re-provisioned sandbox gets a new ClusterIP behind the same
// Service name, so a surviving client would hang against the dead one until
// its context expired.
func (c *Client) Drop(name string) {
	c.mu.Lock()
	cl, ok := c.clients[name]
	delete(c.clients, name)
	c.mu.Unlock()
	if ok {
		closeAll([]*client.Client{cl})
	}
}

func (c *Client) DropAll() {
	c.mu.Lock()
	stale := make([]*client.Client, 0, len(c.clients))
	for name, cl := range c.clients {
		stale = append(stale, cl)
		delete(c.clients, name)
	}
	c.mu.Unlock()
	closeAll(stale)
}

// clientFor builds at most one live client per endpoint, dialing OUTSIDE the
// mutex.
//
// Holding a lock across the dial is the bug that made mcpproxy serialize the
// whole fleet. It cannot bite here — one task, so at worst this sidecar
// blocks itself — but the shape is copied deliberately: the next person to
// read both should not find two different answers to the same question.
func (c *Client) clientFor(ctx context.Context, name string) (*client.Client, error) {
	c.mu.Lock()
	cl, cached := c.clients[name]
	ep, known := c.endpoints[name]
	c.mu.Unlock()
	if cached {
		return cl, nil
	}
	if !known {
		return nil, fmt.Errorf("no endpoint named %q in the roster", name)
	}

	url := fmt.Sprintf("http://%s%s", ep.Address, ep.Path)
	start := time.Now()
	fresh, err := client.NewStreamableHttpClient(url)
	if err != nil {
		return nil, fmt.Errorf("build mcp client for %s: %w", name, err)
	}
	if err := fresh.Start(ctx); err != nil {
		return nil, fmt.Errorf("start mcp client for %s: %w", name, err)
	}
	if _, err := fresh.Initialize(ctx, mcp.InitializeRequest{}); err != nil {
		return nil, fmt.Errorf("initialize mcp client for %s: %w", name, err)
	}
	// Same field name mcpproxy.callExec used, deliberately: docs/adr/0045
	// pins it so the before/after across the cut-over stays a comparison
	// rather than two unrelated numbers.
	slog.Info("e2eclient dial", "endpoint", name, "init_ms", time.Since(start).Milliseconds())

	c.mu.Lock()
	winner, lost := c.clients[name]
	if !lost {
		c.clients[name] = fresh
	}
	c.mu.Unlock()
	if lost {
		closeAll([]*client.Client{fresh})
		return winner, nil
	}
	return fresh, nil
}

func closeAll(clients []*client.Client) {
	for _, cl := range clients {
		if err := cl.Close(); err != nil {
			slog.Error("e2eclient: closing mcp client", "error", err)
		}
	}
}
