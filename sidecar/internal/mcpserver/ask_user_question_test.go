package mcpserver

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type stubAsker struct {
	gotTimeoutMs int32
	answered     bool
	answersJSON  string
	err          error
}

func (s *stubAsker) AskUserQuestion(_ context.Context, _ string, timeoutMs int32) (bool, string, int64, error) {
	s.gotTimeoutMs = timeoutMs
	return s.answered, s.answersJSON, 7, s.err
}

func ask(t *testing.T, a *stubAsker) (*mcp.CallToolResult, error) {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name = "AskUserQuestion"
	req.Params.Arguments = map[string]any{
		"questions": []any{map[string]any{
			"header": "Storage", "question": "which class?",
			"options": []any{map[string]any{"label": "longhorn", "description": "replicated"}},
		}},
	}
	return askUserQuestionHandler(a)(context.Background(), req)
}

func pendingStatus(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) == 0 {
		t.Fatal("no content in tool result")
	}
	text, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("content is %T, want TextContent", res.Content[0])
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(text.Text), &body); err != nil {
		return ""
	}
	return body["status"]
}

// A wait that was cancelled is the PENDING case, not a failure.
//
// The question row is appended by core before the poll starts, so it is live
// and durable whether or not we were still listening when the human answered.
// Returning an error instead says the opposite, and an agent believed it:
// observed live 2026-08-19, the call died at exactly 60s (the MCP client's own
// DEFAULT_REQUEST_TIMEOUT_MSEC, invisible from here), the handler reported a
// failed tool call, and the agent abandoned the tool and pasted its four
// questions into the chat as prose instead.
func TestAskUserQuestion_ACancelledWaitIsPendingNotAnError(t *testing.T) {
	for _, code := range []codes.Code{codes.Canceled, codes.DeadlineExceeded} {
		res, err := ask(t, &stubAsker{err: status.Error(code, "context canceled")})
		if err != nil {
			t.Fatalf("%s surfaced as a tool error: %v — the question is still live, so this is a lie about the state of the world", code, err)
		}
		if got := pendingStatus(t, res); got != "pending" {
			t.Errorf("%s produced status %q, want \"pending\"", code, got)
		}
	}
}

// But a genuinely broken core must still fail loudly — the narrow branch above
// must not become "swallow everything".
func TestAskUserQuestion_ARealFailureIsStillAnError(t *testing.T) {
	if _, err := ask(t, &stubAsker{err: status.Error(codes.Unavailable, "core is down")}); err == nil {
		t.Error("core being unreachable returned success — the agent would end its turn believing a question was posted that never was")
	}
}

// The agent cannot choose how long to wait, and must not be able to: the only
// legal range is "under the transport deadline it cannot see". The knob used to
// exist and its schema told the agent to re-invoke to keep waiting, directly
// contradicting the tool description and docs/adr/0050.
func TestAskUserQuestion_WaitIsFixedAndUnderTheTransportDeadline(t *testing.T) {
	a := &stubAsker{answered: true, answersJSON: `{"answers":{}}`}
	if _, err := ask(t, a); err != nil {
		t.Fatalf("ask: %v", err)
	}
	if a.gotTimeoutMs != askQuestionWaitMs {
		t.Errorf("core was asked to wait %dms, want the fixed %dms", a.gotTimeoutMs, askQuestionWaitMs)
	}
	// 60000 is DEFAULT_REQUEST_TIMEOUT_MSEC in the agent's MCP client. Equalling
	// it races that deadline; exceeding it is the bug this replaced.
	if askQuestionWaitMs >= 60_000 {
		t.Errorf("wait %dms is not comfortably under the MCP client's 60000ms ceiling", askQuestionWaitMs)
	}
}
