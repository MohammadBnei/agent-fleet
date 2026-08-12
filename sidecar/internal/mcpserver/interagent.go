package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/MohammadBnei/agent-fleet/sidecar/internal/coreclient"
)

// Inter-agent coordination tools (docs/adr/0041). Each is a thin proxy to
// core over this pod's single outbound gRPC connection — the agent never
// learns another pod's address, and there is no inter-pod MCP anywhere
// (docs/adr/0020 point 6).

func listSessionsHandler(core *coreclient.Client) server.ToolHandlerFunc {
	return func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sessions, err := core.ListSessions(ctx)
		if err != nil {
			slog.Error("mcp list_sessions", "error", err)
			return mcp.NewToolResultError(err.Error()), nil
		}
		out := make([]map[string]any, 0, len(sessions))
		for _, s := range sessions {
			out = append(out, map[string]any{
				"taskId":      s.GetTaskId(),
				"repo":        s.GetRepo(),
				"description": s.GetDescription(),
				"status":      s.GetStatus(),
				"liveState":   s.GetLiveState(),
			})
		}
		body, err := json.Marshal(map[string]any{"sessions": out})
		if err != nil {
			return nil, fmt.Errorf("list_sessions: marshal: %w", err)
		}
		return mcp.NewToolResultText(string(body)), nil
	}
}

func promptSessionHandler(core *coreclient.Client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		target := req.GetString("taskId", "")
		text := req.GetString("text", "")
		if target == "" || text == "" {
			return mcp.NewToolResultError("taskId and text are required"), nil
		}
		// The agent never sets depth. This is a relay hop count, and a
		// caller that could choose it could choose 0 forever and defeat the
		// loop guard entirely.
		resp, err := core.PromptSession(ctx, target, text, 1)
		if err != nil {
			slog.Error("mcp prompt_agent", "targetTaskId", target, "error", err)
			return mcp.NewToolResultError(err.Error()), nil
		}
		body, err := json.Marshal(map[string]any{
			"seq":       resp.GetSeq(),
			"liveState": resp.GetLiveState(),
			"podName":   resp.GetPodName(),
		})
		if err != nil {
			return nil, fmt.Errorf("prompt_agent: marshal: %w", err)
		}
		slog.Info("mcp prompt_agent", "targetTaskId", target, "seq", resp.GetSeq())
		return mcp.NewToolResultText(string(body)), nil
	}
}

func waitForSessionHandler(core *coreclient.Client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		target := req.GetString("taskId", "")
		if target == "" {
			return mcp.NewToolResultError("taskId is required"), nil
		}
		var until []string
		if raw := req.GetString("until", ""); raw != "" {
			until = []string{raw}
		}
		timeoutMs := int32(req.GetInt("timeoutMs", 120000))
		resp, err := core.WaitForSessionState(ctx, target, until, timeoutMs)
		if err != nil {
			slog.Error("mcp wait_for_agent", "targetTaskId", target, "error", err)
			return mcp.NewToolResultError(err.Error()), nil
		}
		body, err := json.Marshal(map[string]any{
			"liveState": resp.GetLiveState(),
			"timedOut":  resp.GetTimedOut(),
		})
		if err != nil {
			return nil, fmt.Errorf("wait_for_agent: marshal: %w", err)
		}
		return mcp.NewToolResultText(string(body)), nil
	}
}
