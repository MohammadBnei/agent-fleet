package mcpserver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

type fakeReach struct {
	def, input, thread, repo, title, body, since string
	wait, limit                                  int32
	err                                          error
}

func (f *fakeReach) SpawnWorker(_ context.Context, def, input, _ string) (*agentfleetv1.SpawnWorkerResponse, error) {
	f.def, f.input = def, input
	return &agentfleetv1.SpawnWorkerResponse{ThreadId: "t1"}, f.err
}

func (f *fakeReach) WaitForResult(_ context.Context, id string, wait int32) (*agentfleetv1.WaitForResultResponse, error) {
	f.thread, f.wait = id, wait
	return &agentfleetv1.WaitForResultResponse{State: "done", ResultText: "all green", Error: &agentfleetv1.Status{Code: "X", Message: "m"}}, f.err
}

func (f *fakeReach) OpenIssue(_ context.Context, repo, title, body, _ string) (*agentfleetv1.OpenIssueResponse, error) {
	f.repo, f.title, f.body = repo, title, body
	return &agentfleetv1.OpenIssueResponse{Url: "https://github.com/o/r/issues/4"}, f.err
}

func (f *fakeReach) ListResponderCards(_ context.Context, since string, limit int32) (*agentfleetv1.ListResponderCardsResponse, error) {
	f.since, f.limit = since, limit
	return &agentfleetv1.ListResponderCardsResponse{Cards: []*agentfleetv1.ResponderCard{{Id: "c1", Kind: "card", State: "open"}}}, f.err
}

func callTool(t *testing.T, h func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error), args map[string]any) (string, bool) {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	res, err := h(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return res.Content[0].(mcp.TextContent).Text, res.IsError
}

// docs/adr/0084 §6: the four reach tools are registered in every sidecar (core refuses without the grant) and
// forward their arguments, with the session never the agent's to set.
func TestReachTools(t *testing.T) {
	listed := map[string]bool{}
	for _, tl := range newMCPServer(nil, nil).ListTools() {
		listed[tl.Tool.Name] = true
	}
	for _, n := range []string{"spawn_worker", "wait_for_result", "open_issue", "list_responder_cards"} {
		if !listed[n] {
			t.Errorf("%s is not registered", n)
		}
	}
	f := &fakeReach{}
	if txt, isErr := callTool(t, spawnWorkerHandler(f), map[string]any{"definition": "w", "input": "go"}); isErr || !strings.Contains(txt, `"threadId":"t1"`) || f.def != "w" || f.input != "go" {
		t.Errorf("spawn: %q %v %+v", txt, isErr, f)
	}
	if txt, isErr := callTool(t, waitForResultHandler(f), map[string]any{"threadId": "t1", "waitSeconds": 30}); isErr || !strings.Contains(txt, `"state":"done"`) || !strings.Contains(txt, "all green") || f.thread != "t1" || f.wait != 30 {
		t.Errorf("wait: %q %v %+v", txt, isErr, f)
	}
	if txt, isErr := callTool(t, openIssueHandler(f), map[string]any{"repo": "o/r", "title": "T", "body": "B"}); isErr || !strings.Contains(txt, "issues/4") || f.repo != "o/r" || f.title != "T" || f.body != "B" {
		t.Errorf("issue: %q %v %+v", txt, isErr, f)
	}
	if txt, isErr := callTool(t, listResponderCardsHandler(f), map[string]any{"since": "2026-01-01T00:00:00Z", "limit": 5}); isErr || !strings.Contains(txt, `"id":"c1"`) || f.since != "2026-01-01T00:00:00Z" || f.limit != 5 {
		t.Errorf("cards: %q %v %+v", txt, isErr, f)
	}
	for name, h := range map[string]func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error){
		"spawn": spawnWorkerHandler(f), "wait": waitForResultHandler(f), "issue": openIssueHandler(f),
	} {
		if _, isErr := callTool(t, h, map[string]any{}); !isErr {
			t.Errorf("%s with no arguments must be refused here", name)
		}
	}
	f.err = errors.New("core refused")
	for name, h := range map[string]func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error){
		"spawn": spawnWorkerHandler(f), "wait": waitForResultHandler(f), "issue": openIssueHandler(f), "cards": listResponderCardsHandler(f),
	} {
		if txt, isErr := callTool(t, h, map[string]any{"definition": "w", "input": "x", "threadId": "t", "repo": "o/r", "title": "T"}); !isErr || !strings.Contains(txt, "core refused") {
			t.Errorf("%s: core's refusal must reach the agent, got %q", name, txt)
		}
	}
}
