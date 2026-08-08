// Package mcpserver hosts the sidecar's local (localhost-only) MCP server —
// the Agent SDK session's mcpServers config points here, never at core
// directly (docs/adr/0020/0021: MCP is purely local, gRPC is the only
// inter-pod protocol). Every tool here proxies onward to core over the
// sidecar's one outbound gRPC connection. Tool contracts are preserved
// verbatim from core/internal/coreserver's old fleet-core mcpserver and
// provisioner's old per-task mcpserver — just without a taskId parameter,
// since one sidecar only ever serves the one task its pod was created for.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/sidecar/internal/coreclient"
)

type AskUserQuestionOption struct {
	Label       string `json:"label" jsonschema_description:"Short label for this option (1-5 words)"`
	Description string `json:"description" jsonschema_description:"What this option means or its tradeoffs"`
}

type AskUserQuestionQuestion struct {
	Question    string                  `json:"question" jsonschema_description:"The complete question to ask, ending with a question mark"`
	Header      string                  `json:"header" jsonschema_description:"Very short label shown as a chip/tag, max 12 chars"`
	Options     []AskUserQuestionOption `json:"options" jsonschema_description:"2-4 mutually exclusive choices"`
	MultiSelect bool                    `json:"multiSelect,omitempty" jsonschema_description:"Set true to allow selecting more than one option"`
}

type AskUserQuestionArgs struct {
	Questions []AskUserQuestionQuestion `json:"questions" jsonschema_description:"1-4 questions to ask the human"`
	TimeoutMs int                       `json:"timeoutMs,omitempty" jsonschema_description:"How long to block waiting for an answer before returning {\"status\":\"pending\"} (default 60000). Call the tool again with the same questions to keep waiting."`
}

// New builds the sidecar's local MCP HTTP handler. Playwright's tool set
// (discovered at runtime once an e2e session is live) is registered lazily
// by request_e2e_env's handler, mirroring the exact dynamic-registration +
// notifications/tools/list_changed pattern provisioner's old per-task
// mcpserver already used.
func New(core *coreclient.Client) http.Handler {
	s := server.NewMCPServer("agent-fleet-sidecar", "0.1.0", server.WithToolCapabilities(true))

	s.AddTool(mcp.NewTool("send_message",
		mcp.WithDescription("Append a message to this task's shared transcript (visible to the human via the dashboard/Discord relay). The response's nextIndex is the sinceIndex to use if you ever call wait_for_messages afterward — see that tool's own description for why."),
		mcp.WithString("from", mcp.Required(), mcp.Description("'agent' | 'human'")),
		mcp.WithString("text", mcp.Required()),
		mcp.WithString("type", mcp.Description("'discussion' | 'approve' | 'abort'")),
		mcp.WithString("idempotencyKey"),
	), sendMessageHandler(core))

	s.AddTool(mcp.NewTool("wait_for_messages",
		mcp.WithDescription("Block (up to timeoutMs) until new transcript messages appear at or after sinceIndex, then return them. You almost never need this: new human messages already arrive live as your next input while a session is running, with no polling required. sinceIndex is INCLUSIVE and results are NOT filtered by from — if you pass the raw index a prior send_message call returned, you will see that same message come back to you. Always pass the nextIndex field from your last send_message or wait_for_messages response instead."),
		mcp.WithNumber("sinceIndex"),
		mcp.WithNumber("timeoutMs"),
	), waitForMessagesHandler(core))

	s.AddTool(mcp.NewTool("AskUserQuestion",
		mcp.WithDescription("Ask the human one or more structured multiple-choice questions. Answered via the web dashboard. Blocks (up to timeoutMs) until answered, or returns {\"status\":\"pending\"} if it times out first (call again with the same questions to keep waiting). See docs/adr/0018."),
		mcp.WithInputSchema[AskUserQuestionArgs](),
	), askUserQuestionHandler(core))

	s.AddTool(mcp.NewTool("request_e2e_env",
		mcp.WithDescription("Request an on-demand e2e test environment for this task: a live pod running the app on this branch, code-server for human review, and a Playwright MCP server. Returns the preview URL. Safe to call again if one is already running — returns the existing session's URL."),
		mcp.WithString("startCmd", mcp.Description("Shell command that installs deps and starts the app, run via bash -lc from the worktree root (e.g. \"cd front && bun install && bun run dev\"). You know the repo's actual layout better than any static default — supply this whenever the app doesn't live at the worktree root. Omit to use the repo's configured default.")),
	), requestE2eEnvHandler(core, s))

	s.AddTool(mcp.NewTool("kill_env",
		mcp.WithDescription("Tear down this task's e2e environment now, without waiting for the task to finish."),
	), killEnvHandler(core))

	s.AddTool(mcp.NewTool("view_logs",
		mcp.WithDescription("View recent logs from fleet components or deployed apps. Returns formatted log entries. Use this to debug issues with worker, sidecar, or the deployed application during e2e tests. Supports both duration-based queries (duration) and explicit time ranges (start_time/end_time in RFC3339 format)."),
		mcp.WithString("component", mcp.Required(), mcp.Description("Which component: worker|sidecar|core|provisioner|e2e|app")),
		mcp.WithString("app_name", mcp.Description("For component=app, specify app name: dream-analyst|vos-monolith")),
		mcp.WithString("namespace", mcp.Description("Kubernetes namespace (default: agent-fleet, use 'default' for deployed apps)")),
		mcp.WithString("level", mcp.Description("Filter by level: debug|info|warn|error (default: all)")),
		mcp.WithString("duration", mcp.Description("How far back: 1h|30m|6h|24h (default: 1h) - ignored if start_time is set")),
		mcp.WithNumber("limit", mcp.Description("Max entries to return (default 50, max 1000)")),
		mcp.WithString("start_time", mcp.Description("Optional: RFC3339 timestamp (e.g., '2024-01-15T10:30:00Z') - overrides duration")),
		mcp.WithString("end_time", mcp.Description("Optional: RFC3339 timestamp (default: now)")),
	), viewLogsHandler(core))

	return server.NewStreamableHTTPServer(s)
}

func sendMessageHandler(core *coreclient.Client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		from := req.GetString("from", "")
		text := req.GetString("text", "")
		msgType := req.GetString("type", "")
		idempotencyKey := req.GetString("idempotencyKey", "")
		if from == "" || text == "" {
			return mcp.NewToolResultError("from and text are required"), nil
		}
		slog.Info("mcp send_message", "from", from, "type", msgType)
		seq, err := core.SendMessage(ctx, from, text, stringToProtoType(msgType), idempotencyKey)
		if err != nil {
			slog.Error("mcp send_message", "error", err)
			return nil, fmt.Errorf("send_message: %w", err)
		}
		// nextIndex (seq+1) is the exclusive cursor a caller should use for
		// wait_for_messages' sinceIndex — that tool's own sinceIndex is
		// inclusive, so passing this message's raw seq back would echo this
		// same message to its own sender.
		body, _ := json.Marshal(map[string]int64{"index": seq, "nextIndex": seq + 1})
		return mcp.NewToolResultText(string(body)), nil
	}
}

func waitForMessagesHandler(core *coreclient.Client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sinceIndex := int64(req.GetInt("sinceIndex", 0))
		timeoutMs := int32(req.GetInt("timeoutMs", 30000))
		slog.Debug("mcp wait_for_messages", "sinceIndex", sinceIndex, "timeoutMs", timeoutMs)
		entries, nextSeq, err := core.WaitForMessages(ctx, sinceIndex, timeoutMs)
		if err != nil {
			slog.Error("mcp wait_for_messages", "error", err)
			return nil, fmt.Errorf("wait_for_messages: %w", err)
		}
		messages := make([]map[string]any, 0, len(entries))
		for _, e := range entries {
			messages = append(messages, map[string]any{"from": e.GetFrom(), "text": e.GetText(), "type": protoTypeToString(e.GetType())})
		}
		body, _ := json.Marshal(map[string]any{"messages": messages, "nextIndex": nextSeq})
		return mcp.NewToolResultText(string(body)), nil
	}
}

func askUserQuestionHandler(core *coreclient.Client) func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return mcp.NewTypedToolHandler(func(ctx context.Context, _ mcp.CallToolRequest, args AskUserQuestionArgs) (*mcp.CallToolResult, error) {
		if len(args.Questions) == 0 {
			return mcp.NewToolResultError("at least one question is required"), nil
		}
		payload, err := json.Marshal(map[string]any{"questions": args.Questions})
		if err != nil {
			return nil, fmt.Errorf("AskUserQuestion: marshal questions: %w", err)
		}
		timeoutMs := int32(args.TimeoutMs)
		if timeoutMs <= 0 {
			timeoutMs = 60000
		}
		slog.Info("mcp AskUserQuestion", "questions", len(args.Questions))
		status, answersJSON, _, err := core.AskUserQuestion(ctx, string(payload), timeoutMs)
		if err != nil {
			slog.Error("mcp AskUserQuestion", "error", err)
			return nil, fmt.Errorf("AskUserQuestion: %w", err)
		}
		if status == "answered" {
			return mcp.NewToolResultText(answersJSON), nil
		}
		body, _ := json.Marshal(map[string]string{"status": "pending"})
		return mcp.NewToolResultText(string(body)), nil
	})
}

func requestE2eEnvHandler(core *coreclient.Client, s *server.MCPServer) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		startCmd := req.GetString("startCmd", "")
		slog.Info("mcp request_e2e_env", "startCmd", startCmd)
		previewURL, _, err := core.RequestE2eEnv(ctx, startCmd)
		if err != nil {
			slog.Error("mcp request_e2e_env", "error", err)
			return mcp.NewToolResultError(err.Error()), nil
		}

		// New Playwright tools just became reachable — register them and
		// tell the client the tool list changed (see docs/adr/0012's
		// flagged risk: verify this is actually honored by the Agent SDK).
		tools, err := core.ListE2eTools(ctx)
		if err == nil {
			for _, t := range tools {
				addProxiedTool(s, core, t)
			}
			s.SendNotificationToAllClients("notifications/tools/list_changed", nil)
		}

		body, _ := json.Marshal(map[string]string{"url": previewURL})
		return mcp.NewToolResultText(string(body)), nil
	}
}

func addProxiedTool(s *server.MCPServer, core *coreclient.Client, desc *agentfleetv1.E2EToolDescriptor) {
	var schema mcp.ToolInputSchema
	_ = json.Unmarshal([]byte(desc.GetInputSchemaJson()), &schema)
	tool := mcp.Tool{Name: desc.GetName(), Description: desc.GetDescription(), InputSchema: schema}
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		argsJSON, err := json.Marshal(req.GetArguments())
		if err != nil {
			return nil, fmt.Errorf("marshal tool arguments: %w", err)
		}
		resultJSON, isError, err := core.CallE2eTool(ctx, desc.GetName(), string(argsJSON))
		if err != nil {
			return nil, err
		}
		var result mcp.CallToolResult
		if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
			return nil, fmt.Errorf("unmarshal tool result: %w", err)
		}
		result.IsError = isError
		return &result, nil
	})
}

func killEnvHandler(core *coreclient.Client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		slog.Info("mcp kill_env")
		if _, err := core.KillE2eEnv(ctx); err != nil {
			slog.Error("mcp kill_env", "error", err)
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText("kill requested"), nil
	}
}

func stringToProtoType(s string) agentfleetv1.TranscriptEntryType {
	switch s {
	case "discussion":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_DISCUSSION
	case "approve":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_APPROVE
	case "abort":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_ABORT
	case "question":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_QUESTION
	case "answer":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_ANSWER
	default:
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_UNSPECIFIED
	}
}

func protoTypeToString(t agentfleetv1.TranscriptEntryType) string {
	switch t {
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_DISCUSSION:
		return "discussion"
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_APPROVE:
		return "approve"
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_ABORT:
		return "abort"
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_QUESTION:
		return "question"
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_ANSWER:
		return "answer"
	default:
		return ""
	}
}

// LogViewer is the interface for viewing logs. Implemented by *coreclient.Client and mockable in tests.
type LogViewer interface {
	ViewLogs(ctx context.Context, component, appName, namespace, level, duration string, limit int32, startTime, endTime string) (string, error)
}

func viewLogsHandler(core LogViewer) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		component := req.GetString("component", "")
		if component == "" {
			return mcp.NewToolResultError("component is required"), nil
		}

		appName := req.GetString("app_name", "")
		namespace := req.GetString("namespace", "agent-fleet")
		level := req.GetString("level", "")
		duration := req.GetString("duration", "1h")
		limit := int32(req.GetInt("limit", 50))
		startTime := req.GetString("start_time", "")
		endTime := req.GetString("end_time", "")

		slog.Info("mcp view_logs", "component", component, "namespace", namespace, "level", level, "duration", duration, "start_time", startTime, "end_time", endTime)

		logsText, err := core.ViewLogs(ctx, component, appName, namespace, level, duration, limit, startTime, endTime)
		if err != nil {
			slog.Error("mcp view_logs", "error", err)
			return nil, fmt.Errorf("view_logs: %w", err)
		}

		return mcp.NewToolResultText(logsText), nil
	}
}
