//go:build integration

package journal

import (
	"context"
	"testing"
	"time"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
)

// Real Postgres via core/internal/dbtest, which applies db/migrations/ — the
// FTS SQL and the repo == "" branch are exactly the kind of thing a mock
// cannot check, and until issue #198 nothing exercised either of them.

// seed inserts entries at explicit times. created_at has a DEFAULT, so it is
// set directly here rather than via Append — the whole point is the window.
func seed(t *testing.T, s *Store, rows []struct {
	repo, eventType, payload string
	at                       time.Time
}) {
	t.Helper()
	ctx := context.Background()
	for _, r := range rows {
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO knowledge_journal (repo, actor, event_type, payload, created_at)
			VALUES ($1, 'worker', $2, $3, $4)`, r.repo, r.eventType, r.payload, r.at); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

type seedRow = struct {
	repo, eventType, payload string
	at                       time.Time
}

func newStore(t *testing.T) *Store {
	return NewStore(dbtest.NewPool(t))
}

// The query issue #198 exists for: every repo, a date window, no search terms.
// Relevance ranking cannot answer "what happened last week" about entries
// nobody has read yet, so an empty query has to mean chronological — not an
// error, and not a match-nothing predicate.
func TestSearch_EmptyQueryReturnsTheWindowNewestFirst(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	base := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	seed(t, s, []seedRow{
		{"agent-fleet", "agent_note", `{"note":"oldest, before the window"}`, base},
		{"agent-fleet", "agent_note", `{"note":"middle"}`, base.AddDate(0, 0, 3)},
		{"vos-monolith", "session.stopped", `{"note":"newest"}`, base.AddDate(0, 0, 5)},
		{"agent-fleet", "agent_note", `{"note":"after the window"}`, base.AddDate(0, 0, 9)},
	})

	got, err := s.Search(ctx, SearchOpts{
		Since: base.AddDate(0, 0, 1),
		Until: base.AddDate(0, 0, 7),
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("window should hold exactly the 2 middle entries, got %d", len(got))
	}
	// Newest first, deliberately: with a LIMIT, ascending order truncates a
	// seven-day window to its oldest rows and drops what the caller came for.
	if !got[0].CreatedAt.After(got[1].CreatedAt) {
		t.Errorf("entries must come back newest-first, got %v then %v", got[0].CreatedAt, got[1].CreatedAt)
	}
	// And across repos — one call, not one call per repo.
	if got[0].Repo != "vos-monolith" || got[1].Repo != "agent-fleet" {
		t.Errorf("an omitted repo must match every repo, got %q and %q", got[0].Repo, got[1].Repo)
	}
}

// Bound semantics are documented in the proto and the tool schema, so pin
// them: since inclusive, until exclusive. An off-by-one here silently drops a
// day off either end of a report.
func TestSearch_SinceIsInclusiveUntilIsExclusive(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	at := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	seed(t, s, []seedRow{{"agent-fleet", "agent_note", `{"note":"boundary"}`, at}})

	if got, err := s.Search(ctx, SearchOpts{Since: at}); err != nil || len(got) != 1 {
		t.Errorf("since must be inclusive: got %d entries, err %v", len(got), err)
	}
	if got, err := s.Search(ctx, SearchOpts{Until: at}); err != nil || len(got) != 0 {
		t.Errorf("until must be exclusive: got %d entries, err %v", len(got), err)
	}
}

// The pre-existing behaviour, still intact once the query became optional:
// a non-empty query ranks by relevance, and the window still applies to it.
func TestSearch_QueryStillRanksAndNowRespectsTheWindow(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	base := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	seed(t, s, []seedRow{
		{"agent-fleet", "agent_note", `{"note":"migration rollback is irreversible"}`, base},
		{"agent-fleet", "agent_note", `{"note":"unrelated note about pods"}`, base.AddDate(0, 0, 1)},
		{"agent-fleet", "agent_note", `{"note":"migration rollback is irreversible"}`, base.AddDate(0, 0, 20)},
	})

	got, err := s.Search(ctx, SearchOpts{Query: "migration rollback"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("query should match the 2 matching notes, got %d", len(got))
	}

	// Same query, bounded — the window has to survive the ranked path too,
	// which is the half a client-side createdAt filter could never do without
	// ranking having already decided what it got to see.
	got, err = s.Search(ctx, SearchOpts{Query: "migration rollback", Until: base.AddDate(0, 0, 10)})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("a bounded query should match only the in-window note, got %d", len(got))
	}
}

// repo == "" matches every repo (documented on both List and Search); a named
// repo must not leak the others.
func TestSearch_NamedRepoScopes(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	at := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	seed(t, s, []seedRow{
		{"agent-fleet", "agent_note", `{"note":"a"}`, at},
		{"vos-monolith", "agent_note", `{"note":"b"}`, at},
	})

	got, err := s.Search(ctx, SearchOpts{Repo: "vos-monolith"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 || got[0].Repo != "vos-monolith" {
		t.Fatalf("a named repo must scope, got %d entries %+v", len(got), got)
	}
}

// Limit applies to the chronological path too — not only the ranked one.
func TestSearch_LimitApplies(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	at := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	rows := []seedRow{}
	for i := range 5 {
		rows = append(rows, seedRow{"agent-fleet", "agent_note", `{"note":"n"}`, at.Add(time.Duration(i) * time.Hour)})
	}
	seed(t, s, rows)

	got, err := s.Search(ctx, SearchOpts{Limit: 2})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("limit 2 should return 2, got %d", len(got))
	}
}
