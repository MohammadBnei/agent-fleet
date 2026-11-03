package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

type fakeReviewer struct {
	got *agentfleetv1.SubmitReviewRequest
	err error
}

func (f *fakeReviewer) SubmitReview(_ context.Context, req *agentfleetv1.SubmitReviewRequest) (*agentfleetv1.SubmitReviewResponse, error) {
	f.got = req
	if f.err != nil {
		return nil, f.err
	}
	return &agentfleetv1.SubmitReviewResponse{ReviewUrl: "https://github.com/o/r/pull/7#pullrequestreview-1", Event: "COMMENT"}, nil
}

func TestSubmitReviewListed(t *testing.T) {
	var raw json.RawMessage
	for _, tl := range newMCPServer(nil).ListTools() {
		if tl.Tool.Name == "submit_review" {
			raw = tl.Tool.RawInputSchema
		}
	}
	if raw == nil {
		t.Fatal("submit_review is not registered with a typed schema")
	}
	var schema struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	if got := schema.Properties["verdict"].Enum; strings.Join(got, ",") != "approve,changes,comment" {
		t.Errorf("verdict enum = %v", got)
	}
	for _, k := range []string{"repo", "pr", "headSha", "body", "comments"} {
		if _, ok := schema.Properties[k]; !ok {
			t.Errorf("schema has no %s", k)
		}
	}
	// The session is this pod's identity, never the agent's claim.
	if _, ok := schema.Properties["sessionId"]; ok {
		t.Error("submit_review lets the agent set sessionId")
	}
}

func TestSubmitReviewHandler(t *testing.T) {
	args := map[string]any{
		"repo": "o/r", "pr": 7, "headSha": strings.Repeat("a", 40),
		"verdict": "approve", "body": "looks right",
		"comments": []any{map[string]any{"path": "a.go", "line": 3, "body": "nit"}},
	}
	tests := []struct {
		name     string
		err      error
		wantErr  bool
		wantText string
	}{
		{name: "forwards and reports the review", wantText: `"event":"COMMENT"`},
		{name: "relays core's refusal", err: errors.New("SubmitReview: thread is not this PR"), wantErr: true, wantText: "thread is not this PR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeReviewer{err: tt.err}
			req := mcp.CallToolRequest{}
			req.Params.Name = "submit_review"
			req.Params.Arguments = args
			res, err := submitReviewHandler(f)(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if res.IsError != tt.wantErr {
				t.Fatalf("IsError = %v: %+v", res.IsError, res)
			}
			if txt := res.Content[0].(mcp.TextContent).Text; !strings.Contains(txt, tt.wantText) {
				t.Fatalf("result %q, want %q", txt, tt.wantText)
			}
			g := f.got
			if g == nil || g.GetRepo() != "o/r" || g.GetPr() != 7 || g.GetHeadSha() != strings.Repeat("a", 40) ||
				g.GetVerdict() != "approve" || g.GetBody() != "looks right" || len(g.GetComments()) != 1 ||
				g.GetComments()[0].GetPath() != "a.go" || g.GetComments()[0].GetLine() != 3 || g.GetComments()[0].GetBody() != "nit" {
				t.Fatalf("forwarded %v", g)
			}
		})
	}
}
