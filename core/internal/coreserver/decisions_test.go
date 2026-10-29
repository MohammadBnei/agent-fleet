//go:build integration

package coreserver

import (
	"context"
	"testing"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/journal"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

// The supersede of a re-asked question (reuseOrAppendQuestion) is the agent
// withdrawing it (docs/adr/0082 §5): the old decision is withdrawn, never
// answered, and only the new one is open.
func TestReuseOrAppendQuestion_WithdrawsTheSupersededDecision(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := sessions.NewStore(pool)
	tr := transcript.NewPostgresStore(pool)
	tr.SetAppendHook(decisions.OnAppend)
	srv := New(tr, store, journal.NewStore(pool), nil, nil, nil, nil)
	id, err := store.Create(ctx, sessions.CreateParams{Repo: "agent-fleet"})
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := srv.reuseOrAppendQuestion(ctx, id, `{"questions":[{"question":"which class?"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := srv.reuseOrAppendQuestion(ctx, id, `{"questions":[{"question":"which storage class?"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	state := func(seq int64) (s string, answered bool) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT state, answer IS NOT NULL FROM decisions WHERE session_id = $1 AND request_seq = $2`, id, seq).Scan(&s, &answered); err != nil {
			t.Fatal(err)
		}
		return s, answered
	}
	if s, a := state(first); s != "withdrawn" || a {
		t.Errorf("superseded question: %s (answer recorded %v), want withdrawn with none", s, a)
	}
	if s, _ := state(second); s != "open" {
		t.Errorf("re-asked question: %s, want open", s)
	}
	if got, err := store.Get(ctx, id); err != nil || got.PendingDecisions != 1 {
		t.Errorf("pending = %v (%v), want 1", got.PendingDecisions, err)
	}
}
