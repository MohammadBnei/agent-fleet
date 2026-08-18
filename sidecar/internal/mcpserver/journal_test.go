package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
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

// The widened tool made "every entry, every repo, a week wide" expressible in
// one call, and the first live call of it returned 75 KB and blew the context
// budget outright — the row cap (limit, max 500) bounds rows, and an entry
// carries an arbitrary payload_json. ADR-0046: cap at the source, and never
// cap without saying how to reach the rest.
func TestJournalSearch_CapsTheResultAndSaysSo(t *testing.T) {
	big := make([]*agentfleetv1.JournalEntry, 200)
	for i := range big {
		big[i] = &agentfleetv1.JournalEntry{
			Id: int64(i), Repo: "agent-fleet", Actor: "worker", EventType: "agent_note",
			PayloadJson: `{"note":"` + strings.Repeat("x", 500) + `"}`,
			CreatedAt:   "2026-08-17T10:00:00Z",
		}
	}
	res := call(t, &mockJournalSearcher{entries: big}, map[string]any{})
	text := res.Content[0].(mcp.TextContent).Text

	if len(text) > maxJournalBytes*2 {
		t.Errorf("result is %d bytes, expected roughly maxJournalBytes (%d)", len(text), maxJournalBytes)
	}

	// Still parseable. Cutting bytes mid-JSON would not be.
	var body struct {
		Entries   []map[string]any `json:"entries"`
		Truncated string           `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(text), &body); err != nil {
		t.Fatalf("a capped result must still be valid JSON: %v", err)
	}
	if len(body.Entries) == 0 || len(body.Entries) == len(big) {
		t.Fatalf("expected a trimmed-but-non-empty entry list, got %d of %d", len(body.Entries), len(big))
	}
	if body.Truncated == "" {
		t.Fatal("silent truncation is the failure mode this guards — the result must say it was capped")
	}
	// Naming the way back is the actual contract (ADR-0046), not the cap.
	for _, knob := range []string{"repo", "since", "query", "limit"} {
		if !strings.Contains(body.Truncated, knob) {
			t.Errorf("the notice should name %q as a way to narrow the query", knob)
		}
	}
	// Newest-first means the kept end is the recent one.
	if body.Entries[0]["id"] != float64(0) {
		t.Errorf("trimming must drop the tail, not the head; first id = %v", body.Entries[0]["id"])
	}
}

// An uncapped result must not grow a notice claiming it was capped.
func TestJournalSearch_SmallResultIsNotMarkedTruncated(t *testing.T) {
	res := call(t, &mockJournalSearcher{entries: []*agentfleetv1.JournalEntry{{
		Id: 1, Repo: "agent-fleet", Actor: "worker", EventType: "agent_note", PayloadJson: `{"note":"x"}`,
	}}}, map[string]any{})
	if strings.Contains(res.Content[0].(mcp.TextContent).Text, "truncated") {
		t.Error("a result that fits must not claim to be truncated")
	}
}
