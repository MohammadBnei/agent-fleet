package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

type stubWaiter struct{ gotTimeoutMs int32 }

func (s *stubWaiter) WaitForMessages(_ context.Context, _ int64, timeoutMs int32) ([]*agentfleetv1.TranscriptEntry, int64, error) {
	s.gotTimeoutMs = timeoutMs
	return nil, 0, nil
}

func (s *stubWaiter) WaitForSessionState(_ context.Context, _ string, _ []string, timeoutMs int32, _ int64) (*agentfleetv1.WaitForSessionStateResponse, error) {
	s.gotTimeoutMs = timeoutMs
	return &agentfleetv1.WaitForSessionStateResponse{}, nil
}

// The agent asks for two minutes; core must still be told the fixed wait.
// A zero here would be worse than the old knob: core treats <=0 as "use my
// own default", and both of those defaults sit above the MCP client's 60s
// ceiling — the docs/adr/0058 incident again, with no argument at all.
func TestBlockingTools_IgnoreACallerTimeoutAndSendTheFixedWait(t *testing.T) {
	for name, handler := range map[string]func(*stubWaiter) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error){
		"wait_for_messages": func(w *stubWaiter) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return waitForMessagesHandler(w)
		},
		"wait_for_agent": func(w *stubWaiter) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return waitForSessionHandler(w)
		},
	} {
		w := &stubWaiter{}
		req := mcp.CallToolRequest{}
		req.Params.Name = name
		req.Params.Arguments = map[string]any{"sessionId": "other", "timeoutMs": 120000}
		if _, err := handler(w)(context.Background(), req); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if w.gotTimeoutMs != blockingWaitMs {
			t.Errorf("%s sent core %dms, want the fixed %dms", name, w.gotTimeoutMs, blockingWaitMs)
		}
	}
}

// The description is what the agent actually reads. It said "Blocks (up to
// timeoutMs)" for a month after the argument was deleted.
func TestBlockingTools_NoDescriptionOffersATimeout(t *testing.T) {
	// A nil client is fine: registration never dials core.
	for _, tool := range newMCPServer(nil).ListTools() {
		desc := tool.Tool.Description
		if strings.Contains(desc, "timeoutMs") {
			t.Errorf("%s description still mentions timeoutMs: %q", tool.Tool.Name, desc)
		}
		if _, ok := tool.Tool.InputSchema.Properties["timeoutMs"]; ok {
			t.Errorf("%s still accepts a timeoutMs argument", tool.Tool.Name)
		}
	}
}
