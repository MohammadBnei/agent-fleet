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

type fakeDrafter struct {
	got *agentfleetv1.DraftCardRequest
	err error
}

func (f *fakeDrafter) DraftCard(_ context.Context, req *agentfleetv1.DraftCardRequest) (*agentfleetv1.DraftCardResponse, error) {
	f.got = req
	if f.err != nil {
		return nil, f.err
	}
	return &agentfleetv1.DraftCardResponse{DecisionId: "d1", ExpiresAt: "2026-11-06T00:00:00Z"}, nil
}

func TestDraftCardListed(t *testing.T) {
	var raw json.RawMessage
	for _, tl := range newMCPServer(nil).ListTools() {
		if tl.Tool.Name == "draft_card" {
			raw = tl.Tool.RawInputSchema
		}
	}
	if raw == nil {
		t.Fatal("draft_card is not registered with a typed schema")
	}
	var schema struct {
		Properties map[string]struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	if got := schema.Properties["subject"].Properties["kind"].Enum; strings.Join(got, ",") != "pr,issue,alert,repo" {
		t.Errorf("subject.kind enum = %v", got)
	}
	for _, k := range []string{"ask", "options", "writeOwn"} {
		if _, ok := schema.Properties[k]; !ok {
			t.Errorf("schema has no %s", k)
		}
	}
	// Core fills these; the agent must not be able to claim them.
	for _, k := range []string{"sessionId", "expiresAt", "writerDefinition", "draftedBy"} {
		if _, ok := schema.Properties[k]; ok {
			t.Errorf("draft_card lets the agent set %s", k)
		}
	}
}

func TestDraftCardHandler(t *testing.T) {
	args := map[string]any{
		"subject": map[string]any{"repo": "o/r", "kind": "pr", "number": 7},
		"ask":     "merge or close?",
		"options": []any{
			map[string]any{"label": "Merge", "effect": map[string]any{"verb": "handoff"}, "reversible": "partial", "recommended": true},
			map[string]any{"label": "Close", "effect": map[string]any{"verb": "close_threads"}, "reversible": "no"},
		},
		"writeOwn":       true,
		"idempotencyKey": "k",
	}
	tests := []struct {
		name     string
		err      error
		wantErr  bool
		wantText string
	}{
		{name: "forwards and reports open", wantText: `"status":"open"`},
		{name: "relays core's refusal", err: errors.New("DraftCard: core.card_budget: 10 cards in 24h"), wantErr: true, wantText: "core.card_budget"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeDrafter{err: tt.err}
			req := mcp.CallToolRequest{}
			req.Params.Name = "draft_card"
			req.Params.Arguments = args
			res, err := draftCardHandler(f)(context.Background(), req)
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
			if g == nil || g.GetAsk() != "merge or close?" || g.GetSubject().GetNumber() != 7 || g.GetSubject().GetKind() != "pr" ||
				len(g.GetOptions()) != 2 || g.GetOptions()[1].GetEffect().GetVerb() != "close_threads" ||
				!g.GetOptions()[0].GetRecommended() || !g.GetWriteOwn() || g.GetIdempotencyKey() != "k" {
				t.Fatalf("forwarded %v", g)
			}
		})
	}
}
