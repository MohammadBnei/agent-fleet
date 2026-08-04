// Package mcpserver exposes the planning transcript over MCP
// (send_message/wait_for_messages) — the same tool contract
// worker/src/planning.ts's proposer/critic sessions already call today
// against mcp-redis, preserved verbatim to minimize prompt churn (see
// docs/adr/0013). This is the only transport worker ever speaks to
// fleet-core: never gRPC.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/MohammadBnei/agent-fleet/fleet-core/internal/transcript"
)

const pollInterval = 500 * time.Millisecond

// New builds the fleet-core MCP HTTP handler (mounted at /mcp) backed by
// store.
func New(store transcript.Store) http.Handler {
	s := server.NewMCPServer("agent-fleet-core", "0.1.0")

	s.AddTool(mcp.NewTool("send_message",
		mcp.WithDescription("Append a message to this task's shared planning transcript (visible to proposer, critic, and the human via Discord relay)."),
		mcp.WithString("taskId", mcp.Required()),
		mcp.WithString("from", mcp.Required(), mcp.Description("'proposer' | 'critic' | 'human'")),
		mcp.WithString("text", mcp.Required()),
		mcp.WithString("type", mcp.Description("'discussion' | 'approve' | 'abort'")),
		mcp.WithString("idempotencyKey"),
	), sendMessageHandler(store))

	s.AddTool(mcp.NewTool("wait_for_messages",
		mcp.WithDescription("Block (up to timeoutMs) until new planning-transcript messages appear after sinceIndex, then return them. Never misses messages sent while not waiting."),
		mcp.WithString("taskId", mcp.Required()),
		mcp.WithNumber("sinceIndex"),
		mcp.WithNumber("timeoutMs"),
	), waitForMessagesHandler(store))

	return server.NewStreamableHTTPServer(s)
}

func sendMessageHandler(store transcript.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		taskID := req.GetString("taskId", "")
		from := req.GetString("from", "")
		text := req.GetString("text", "")
		msgType := req.GetString("type", "")
		// Empty idempotencyKey is fine here — Store.Append mints a fresh
		// UUID itself when one isn't supplied (single source of truth for
		// that guarantee, not duplicated in every caller). The LLM
		// tool-call itself isn't a mechanical "retry" the server can
		// distinguish from a genuinely new message anyway; the key mostly
		// matters for mechanically-retried callers (network client,
		// Discord interaction.id).
		idempotencyKey := req.GetString("idempotencyKey", "")
		if taskID == "" || from == "" || text == "" {
			return mcp.NewToolResultError("taskId, from, and text are required"), nil
		}

		seq, err := store.Append(ctx, taskID, from, text, msgType, idempotencyKey)
		if err != nil {
			return nil, fmt.Errorf("send_message: %w", err)
		}
		body, _ := json.Marshal(map[string]int64{"index": seq})
		return mcp.NewToolResultText(string(body)), nil
	}
}

func waitForMessagesHandler(store transcript.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		taskID := req.GetString("taskId", "")
		if taskID == "" {
			return mcp.NewToolResultError("taskId is required"), nil
		}
		sinceIndex := int64(req.GetInt("sinceIndex", 0))
		timeoutMs := req.GetInt("timeoutMs", 30000)

		deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
		for {
			entries, nextSeq, err := store.ReadSince(ctx, taskID, sinceIndex, 1000)
			if err != nil {
				return nil, fmt.Errorf("wait_for_messages: %w", err)
			}
			if len(entries) > 0 || time.Now().After(deadline) {
				return toolResult(entries, nextSeq, sinceIndex)
			}
			select {
			case <-ctx.Done():
				return toolResult(nil, sinceIndex, sinceIndex)
			case <-time.After(pollInterval):
			}
		}
	}
}

func toolResult(entries []transcript.Entry, nextSeq, sinceIndex int64) (*mcp.CallToolResult, error) {
	messages := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		messages = append(messages, map[string]any{"from": e.From, "text": e.Text, "type": e.Type})
	}
	if len(entries) == 0 {
		nextSeq = sinceIndex
	}
	body, _ := json.Marshal(map[string]any{"messages": messages, "nextIndex": nextSeq})
	return mcp.NewToolResultText(string(body)), nil
}
