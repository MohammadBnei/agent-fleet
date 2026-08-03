// Package mcpserver is the Go port of e2e-provisioner/src/http.ts: one MCP
// server per task at /mcp/:taskId — the single stable endpoint the
// worker's Agent SDK session talks to for the whole implementation phase.
// It exposes request_e2e_env/kill_env directly, and once a pod is live for
// that task, also whatever Playwright tools mcpproxy reports.
package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/MohammadBnei/agent-fleet/e2e-provisioner/internal/db"
	"github.com/MohammadBnei/agent-fleet/e2e-provisioner/internal/k8s"
	"github.com/MohammadBnei/agent-fleet/e2e-provisioner/internal/mcpproxy"
)

var pathRe = regexp.MustCompile(`^/mcp/([^/?]+)`)

type Handler struct {
	store   *db.Store
	k8sc    *k8s.Client
	proxy   *mcpproxy.Proxy
	e2eHost string

	mu       sync.Mutex
	handlers map[string]http.Handler
}

func New(store *db.Store, k8sc *k8s.Client, proxy *mcpproxy.Proxy, e2eHost string) *Handler {
	return &Handler{store: store, k8sc: k8sc, proxy: proxy, e2eHost: e2eHost, handlers: make(map[string]http.Handler)}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	match := pathRe.FindStringSubmatch(r.URL.Path)
	if match == nil {
		http.NotFound(w, r)
		return
	}
	taskID := match[1]
	h.handlerFor(taskID).ServeHTTP(w, r)
}

func (h *Handler) handlerFor(taskID string) http.Handler {
	h.mu.Lock()
	defer h.mu.Unlock()
	if existing, ok := h.handlers[taskID]; ok {
		return existing
	}
	s := h.buildServer(taskID)
	sh := server.NewStreamableHTTPServer(s)
	h.handlers[taskID] = sh
	return sh
}

func (h *Handler) buildServer(taskID string) *server.MCPServer {
	s := server.NewMCPServer("agent-fleet-e2e", "0.1.0", server.WithToolCapabilities(true))

	s.AddTool(mcp.NewTool("request_e2e_env",
		mcp.WithDescription("Request an on-demand e2e test environment for this task: a live pod running the app on this branch, code-server for human review, and a Playwright MCP server. Returns the preview URL. Safe to call again if one is already running — returns the existing session's URL."),
	), h.requestE2eEnvHandler(taskID, s))

	s.AddTool(mcp.NewTool("kill_env",
		mcp.WithDescription("Tear down this task's e2e environment now, without waiting for the task to finish."),
	), h.killEnvHandler(taskID))

	for _, tool := range h.proxy.ProxiedTools(context.Background(), taskID) {
		h.addProxiedTool(s, taskID, tool)
	}

	return s
}

func (h *Handler) addProxiedTool(s *server.MCPServer, taskID string, tool mcp.Tool) {
	name := tool.Name
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return h.proxy.CallTool(ctx, taskID, name, req.GetArguments())
	})
}

func (h *Handler) requestE2eEnvHandler(taskID string, s *server.MCPServer) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		url, err := h.requestE2eEnv(ctx, taskID)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		// New Playwright tools just became reachable — register them and
		// tell the client the tool list changed so the Agent SDK re-fetches
		// (see docs/adr/0012's flagged risk: verify this is actually
		// honored, since it's load-bearing for "tools appear after the pod
		// exists").
		for _, tool := range h.proxy.ProxiedTools(ctx, taskID) {
			h.addProxiedTool(s, taskID, tool)
		}
		s.SendNotificationToAllClients("notifications/tools/list_changed", nil)

		body, _ := json.Marshal(map[string]string{"url": url})
		return mcp.NewToolResultText(string(body)), nil
	}
}

func (h *Handler) requestE2eEnv(ctx context.Context, taskID string) (string, error) {
	existing, err := h.store.GetActiveSessionForTask(ctx, taskID)
	if err != nil {
		return "", err
	}
	if existing != nil {
		return k8s.PreviewURLFor(h.e2eHost, taskID), nil
	}

	task, err := h.store.GetTask(ctx, taskID)
	if err != nil {
		return "", err
	}
	if task == nil {
		return "", errUnknownTask(taskID)
	}

	sess, err := h.store.CreateSession(ctx, taskID)
	if err != nil {
		return "", err
	}
	if err := h.k8sc.CreatePod(ctx, k8s.TaskRef{ID: taskID, Repo: task.Repo}); err != nil {
		_ = h.store.SetSessionStatus(ctx, sess.ID, "failed", nil, nil)
		return "", err
	}
	if err := h.k8sc.CreateService(ctx, taskID); err != nil {
		_ = h.store.SetSessionStatus(ctx, sess.ID, "failed", nil, nil)
		return "", err
	}
	if err := h.k8sc.CreateMiddleware(ctx, taskID); err != nil {
		_ = h.store.SetSessionStatus(ctx, sess.ID, "failed", nil, nil)
		return "", err
	}
	if err := h.k8sc.CreateIngressRoute(ctx, h.e2eHost, taskID); err != nil {
		_ = h.store.SetSessionStatus(ctx, sess.ID, "failed", nil, nil)
		return "", err
	}

	podName := k8s.ResourceName(taskID)
	ingressPath := "/" + taskID
	if err := h.store.SetSessionStatus(ctx, sess.ID, "running", &podName, &ingressPath); err != nil {
		return "", err
	}
	return k8s.PreviewURLFor(h.e2eHost, taskID), nil
}

func (h *Handler) killEnvHandler(taskID string) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if _, err := h.store.RequestKill(ctx, taskID); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText("kill requested"), nil
	}
}

type errUnknownTask string

func (e errUnknownTask) Error() string {
	return "unknown task " + strings.TrimSpace(string(e))
}
