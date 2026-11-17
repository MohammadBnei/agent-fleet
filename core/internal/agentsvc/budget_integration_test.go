//go:build integration

package agentsvc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/MohammadBnei/agent-fleet/core/internal/dashboard"
	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/metrics"
	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1/agentfleetv1connect"
)

// docs/adr/0084 §9: a character that spent its day's turns gets one card a day; answering its raise_budget option
// lifts the hold for 24 hours; a card cannot raise another character's budget.
func TestReportBudgets_CardAndRaise(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := sessions.NewStore(pool)
	transcr := transcript.NewPostgresStore(pool, decisions.OnAppend)
	for _, n := range []string{"d", "e"} {
		if _, err := pool.Exec(ctx, `INSERT INTO agent_definitions (name, sha, spec, effective, valid) VALUES ($1, 'sha1', '{}', '{"limits":{"turnsPerDay":1}}', true)`, n); err != nil {
			t.Fatal(err)
		}
	}
	mk := func(n string) string {
		id, err := store.Create(ctx, sessions.CreateParams{Repo: "r", PermissionMode: "default", DefinitionName: n, DefinitionSHA: "sha1",
			Owner: "client:standing:" + n, Visibility: "private", Lifecycle: sessions.Standing, Invoker: "standing:" + n, ThreadKey: "main", StartedBy: "root"})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	d, e := mk("d"), mk("e")
	if _, err := pool.Exec(ctx, `INSERT INTO transcript (session_id, seq, "from", text, type, idempotency_key, thread_key)
		VALUES ($1, 1, 'worker', '{"subtype":"success"}', 'result', gen_random_uuid()::text, 'main')`, d); err != nil {
		t.Fatal(err)
	}
	decs := decisions.NewStore(pool)
	dash := &noWarm{store: store, Server: dashboard.NewServer(store, nil, transcr, nil, repos.NewStore(pool), nil, nil, nil, sessions.Caps{MaxLive: 5}, nil, nil, nil, "test", "")}
	s := &Server{definitions: fakeHeads{{Name: "d", SHA: "sha1"}}, dash: dash, sessions: store, transcr: transcr, hub: dashboard.NewHub(), decisions: decs}
	path, h := agentfleetv1connect.NewAgentServiceHandler(s, connect.WithInterceptors(asSub{}))
	mux := http.NewServeMux()
	mux.Handle(path, h)
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)
	c := agentfleetv1connect.NewAgentServiceClient(hs.Client(), hs.URL)

	cards := func(id string) (n int, card string) {
		_ = pool.QueryRow(ctx, `SELECT count(*), COALESCE(max(id::text), '') FROM decisions WHERE session_id = $1 AND kind = 'card'`, id).Scan(&n, &card)
		return
	}
	for i := 0; i < 2; i++ { // the second pass files nothing new
		if err := s.ReportBudgets(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := cards(d); n != 1 {
		t.Fatalf("a character at its budget has %d cards, want 1", n)
	}
	// The alert's two gauges: d spent one result in the hour against a budget of 1.
	if g := testutil.ToFloat64(metrics.CharacterTurnsLastHour.WithLabelValues("d")); g != 1 {
		t.Errorf("turns_last_hour = %v, want 1", g)
	}
	if g := testutil.ToFloat64(metrics.CharacterTurnsPerDay.WithLabelValues("d")); g != 1 {
		t.Errorf("turns_per_day = %v, want 1", g)
	}
	if n, _ := cards(e); n != 0 {
		t.Fatalf("a character under its budget has %d cards, want 0", n)
	}
	if held, _ := store.BudgetHeld(ctx, d); !held {
		t.Fatal("not held at its budget")
	}
	_, id := cards(d)
	if _, err := c.AnswerDecision(ctx, as("root", &agentfleetv1.AnswerDecisionRequest{DecisionId: id, OptionId: "1"})); err != nil {
		t.Fatal(err)
	}
	if held, _ := store.BudgetHeld(ctx, d); held {
		t.Fatal("still held after raise_budget")
	}
	if dash.warms != nil {
		t.Errorf("answering a budget card woke %v", dash.warms)
	}
	var ctxOnly bool
	if err := pool.QueryRow(ctx, `SELECT context_only FROM transcript WHERE session_id = $1 AND type = 'answer'`, d).Scan(&ctxOnly); err != nil || !ctxOnly {
		t.Errorf("the budget answer reaches the character: context_only = %v, %v", ctxOnly, err)
	}
	if err := s.ReportBudgets(ctx); err != nil {
		t.Fatal(err)
	}

	// Only core files a character card.
	if _, err := decs.Draft(ctx, transcr, decisions.Draft{SessionID: e, From: "agent", Ask: "Lift it?",
		Subject: &agentfleetv1.CardSubject{Kind: "character", Repo: "e"},
		Options: []*agentfleetv1.CardOption{
			{Label: "Raise", Reversible: "yes", Effect: &agentfleetv1.CardEffect{Verb: "handoff"}},
			{Label: "No", Reversible: "yes", Effect: &agentfleetv1.CardEffect{Verb: "handoff"}},
		}}); err == nil {
		t.Error("an agent filed a character card")
	}

	// A card on e's session naming d raises nothing.
	r, err := decs.Draft(ctx, transcr, decisions.Draft{SessionID: e, From: "system", Ask: "Lift it?",
		Subject: &agentfleetv1.CardSubject{Kind: "character", Repo: "e"},
		Options: []*agentfleetv1.CardOption{
			{Label: "Raise", Reversible: "yes", Effect: &agentfleetv1.CardEffect{Verb: decisions.VerbRaiseBudget, Payload: `{"character":"d"}`}},
			{Label: "No", Reversible: "yes", Effect: &agentfleetv1.CardEffect{Verb: "handoff"}},
		}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AnswerDecision(ctx, as("root", &agentfleetv1.AnswerDecisionRequest{DecisionId: r.ID, OptionId: "1"})); err == nil {
		t.Error("a card raised the budget of a character that is not its writer")
	}
	var until *string
	_ = pool.QueryRow(ctx, `SELECT budget_override_until::text FROM sessions WHERE id = $1`, e).Scan(&until)
	if until != nil {
		t.Errorf("e's override = %v, want none", *until)
	}

	// "Keep the hold" leaves the character held, wakes nothing and starts no turn for it.
	keep, err := decs.Draft(ctx, transcr, decisions.Draft{SessionID: d, From: "system", Ask: "Lift it?", IdempotencyKey: "keep",
		Subject: &agentfleetv1.CardSubject{Kind: "character", Repo: "d"},
		Options: []*agentfleetv1.CardOption{
			{Label: "Keep the hold", Reversible: "yes", Effect: &agentfleetv1.CardEffect{Verb: "handoff"}},
			{Label: "Other", Reversible: "yes", Effect: &agentfleetv1.CardEffect{Verb: "handoff"}},
		}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sessions SET budget_override_until = NULL WHERE id = $1`, d); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AnswerDecision(ctx, as("root", &agentfleetv1.AnswerDecisionRequest{DecisionId: keep.ID, OptionId: "1"})); err != nil {
		t.Fatal(err)
	}
	if held, _ := store.BudgetHeld(ctx, d); !held {
		t.Error("not held after Keep the hold")
	}
	if dash.warms != nil {
		t.Errorf("Keep the hold woke %v", dash.warms)
	}
	var turns int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM transcript WHERE session_id = $1 AND type = 'answer' AND NOT context_only`, d).Scan(&turns)
	if turns != 0 {
		t.Errorf("%d answers reach the character", turns)
	}
}
