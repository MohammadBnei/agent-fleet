package mcpserver

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/sidecar/internal/coreclient"
)

type mockJournalSearcher struct {
	got     coreclient.JournalSearch
	entries []*agentfleetv1.JournalEntry
}

func (m *mockJournalSearcher) SearchJournal(_ context.Context, s coreclient.JournalSearch) ([]*agentfleetv1.JournalEntry, error) {
	m.got = s
	return m.entries, nil
}

func call(t *testing.T, mock *mockJournalSearcher, params map[string]any) *mcp.CallToolResult {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name = "journal_search"
	req.Params.Arguments = params
	res, err := journalSearchHandler(mock)(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	return res
}

// The whole point of issue #198: "every entry, all repos, last seven days" has
// to be expressible. Before this, repo and query were both mcp.Required() and
// the handler rejected the call outright, so the weekly rundown curled the
// dashboard's own API from a worker pod instead.
func TestJournalSearch_NoArgsIsNotAnError(t *testing.T) {
	mock := &mockJournalSearcher{}
	if res := call(t, mock, map[string]any{}); res.IsError {
		t.Fatalf("a call with no arguments must succeed, got error result: %+v", res.Content)
	}
	if mock.got.Repo != "" || mock.got.Query != "" {
		t.Errorf("omitted repo/query must reach core as empty (= all repos, no predicate), got %+v", mock.got)
	}
	if mock.got.Limit != 20 {
		t.Errorf("default limit should be 20, got %d", mock.got.Limit)
	}
}

// since/until are two adjacent same-typed strings — the exact shape of the
// SaveAgentSessionId swap in CLAUDE.md's trap list. Assert they land in the
// right fields, not merely that both are non-empty.
func TestJournalSearch_WindowReachesCoreUnswapped(t *testing.T) {
	mock := &mockJournalSearcher{}
	call(t, mock, map[string]any{
		"repo":  "agent-fleet",
		"since": "2026-08-11",
		"until": "2026-08-18T00:00:00Z",
		"limit": float64(100),
	})
	want := coreclient.JournalSearch{
		Repo: "agent-fleet", Since: "2026-08-11", Until: "2026-08-18T00:00:00Z", Limit: 100,
	}
	if mock.got != want {
		t.Errorf("core got %+v, want %+v", mock.got, want)
	}
}

// With repo optional, an entry whose repo the caller cannot see is not usable
// — the result used to carry only actor/eventType/payload/createdAt.
func TestJournalSearch_ResultCarriesRepoAndID(t *testing.T) {
	mock := &mockJournalSearcher{entries: []*agentfleetv1.JournalEntry{{
		Id: 42, Repo: "vos-monolith", Actor: "worker",
		EventType: "agent_note", PayloadJson: `{"note":"x"}`, CreatedAt: "2026-08-17T10:00:00Z",
	}}}
	res := call(t, mock, map[string]any{})
	text, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", res.Content[0])
	}
	var body struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal([]byte(text.Text), &body); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(body.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(body.Entries))
	}
	if body.Entries[0]["repo"] != "vos-monolith" {
		t.Errorf("entry must carry its repo, got %v", body.Entries[0]["repo"])
	}
	if body.Entries[0]["id"] != float64(42) {
		t.Errorf("entry must carry its id, got %v", body.Entries[0]["id"])
	}
}
