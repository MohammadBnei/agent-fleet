//go:build integration

package coreserver

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

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
	tr := transcript.NewPostgresStore(pool, decisions.OnAppend)
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

func nbServer(t *testing.T) (context.Context, *Server, *sessions.Store, string, func(seq int64) string) {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := sessions.NewStore(pool)
	tr := transcript.NewPostgresStore(pool, decisions.OnAppend)
	srv := New(tr, store, journal.NewStore(pool), nil, nil, nil, nil)
	srv.SetDecisions(decisions.NewStore(pool))
	id, err := store.Create(ctx, sessions.CreateParams{Repo: "agent-fleet"})
	if err != nil {
		t.Fatal(err)
	}
	state := func(seq int64) string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, `SELECT state FROM decisions WHERE session_id = $1 AND request_seq = $2`, id, seq).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	return ctx, srv, store, id, state
}

func (s *Server) ask(t *testing.T, ctx context.Context, id, text string) int64 {
	t.Helper()
	r, err := s.SendMessage(ctx, &agentfleetv1.SendMessageRequest{
		SessionId: id, From: "worker", Text: text, IdempotencyKey: uuid.NewString(),
		Type: agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_PERMISSION_REQUEST,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r.GetSeq()
}

// docs/adr/0082 §5: a non-blocking worker's same call asked again, while its
// decision is open, is that decision; an older worker's is asked anew.
func TestSendMessage_StampedRequestIsAskedOnce(t *testing.T) {
	ctx, srv, store, id, state := nbServer(t)
	announced := 0
	srv.SetNotifyBlockedFunc(func(context.Context, string, string, string, string) error { announced++; return nil })

	first := srv.ask(t, ctx, id, `{"tool":"Bash","input":{"command":"ls","n":1},"nb":1}`)
	again := srv.ask(t, ctx, id, `{"nb":1,"input":{"n":1,"command":"ls"},"tool":"Bash"}`)
	if again != first || announced != 1 {
		t.Errorf("same call: seq %d then %d, announced %d; want one seq, announced once", first, again, announced)
	}
	if other := srv.ask(t, ctx, id, `{"tool":"Bash","input":{"command":"pwd"},"nb":1}`); other == first {
		t.Error("another input reused the decision")
	}
	old := srv.ask(t, ctx, id, `{"tool":"Edit","input":{"f":1}}`)
	if oldAgain := srv.ask(t, ctx, id, `{"tool":"Edit","input":{"f":1}}`); oldAgain == old {
		t.Error("an older worker's request was deduplicated")
	}
	if got, err := store.Get(ctx, id); err != nil || got.PendingDecisions != 4 {
		t.Errorf("pending = %v (%v), want 4", got.PendingDecisions, err)
	}
	if state(first) != "open" {
		t.Errorf("first: %s", state(first))
	}
}

// A new stamped plan supersedes the session's open plan: withdrawn by an
// agent-authored reply, never answered; the same plan again is the open one.
func TestSendMessage_StampedPlanSupersedes(t *testing.T) {
	ctx, srv, store, id, state := nbServer(t)
	first := srv.ask(t, ctx, id, `{"tool":"ExitPlanMode","input":{"plan":"a"},"nb":1}`)
	if again := srv.ask(t, ctx, id, `{"tool":"ExitPlanMode","input":{"plan":"a"},"nb":1}`); again != first {
		t.Errorf("the same plan: %d, want %d", again, first)
	}
	second := srv.ask(t, ctx, id, `{"tool":"ExitPlanMode","input":{"plan":"b"},"nb":1}`)
	if state(first) != "withdrawn" || state(second) != "open" {
		t.Errorf("first %s (want withdrawn), second %s (want open)", state(first), state(second))
	}
	entries, _, err := srv.transcr.ReadSince(ctx, id, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.ReplyTo != nil && *e.ReplyTo == first && (e.From != "agent" || e.Type != "permission_response" || e.Seq > second) {
			t.Errorf("supersede reply %+v: want an agent permission_response before the new plan", e)
		}
	}
	// An older worker's plan supersedes nothing.
	srv.ask(t, ctx, id, `{"tool":"ExitPlanMode","input":{"plan":"c"}}`)
	if state(second) != "open" {
		t.Errorf("an unstamped plan superseded %d: %s", second, state(second))
	}
	if got, _ := store.Get(ctx, id); got.PendingDecisions != 2 {
		t.Errorf("pending = %d, want 2", got.PendingDecisions)
	}
}

// The pending reply names the question's decision.
func TestAskUserQuestion_PendingNamesItsDecision(t *testing.T) {
	ctx, srv, _, id, _ := nbServer(t)
	resp, err := srv.AskUserQuestion(ctx, &agentfleetv1.AskUserQuestionRequest{SessionId: id, QuestionsJson: `{"questions":[{"question":"q?"}]}`, TimeoutMs: 10})
	if err != nil {
		t.Fatal(err)
	}
	r, err := srv.decisions.Request(ctx, id, resp.GetQuestionSeq())
	if err != nil || resp.GetAnswered() || resp.GetDecisionId() != r.ID {
		t.Errorf("decision_id %q, want %q (%v)", resp.GetDecisionId(), r.ID, err)
	}
}

// GetSession tells the worker DECISION_WAIT_MS.
func TestGetSession_DecisionWait(t *testing.T) {
	ctx, srv, _, id, _ := nbServer(t)
	srv.SetDecisionWait(90 * time.Second)
	r, err := srv.GetSession(ctx, &agentfleetv1.GetSessionRequest{Id: id})
	if err != nil || r.GetDecisionWaitMs() != 90000 {
		t.Errorf("decision_wait_ms %d (%v), want 90000", r.GetDecisionWaitMs(), err)
	}
}

// prompt_agent reaches a session with an open decision: nothing blocks.
func TestPromptSession_ReachesASessionWithAnOpenDecision(t *testing.T) {
	ctx, srv, store, target, _ := nbServer(t)
	caller, err := store.Create(ctx, sessions.CreateParams{Repo: "agent-fleet"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetPodPhase(ctx, target, "POD_PHASE_RUNNING", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.TouchActive(ctx, target, "worker", "permission_request"); err != nil {
		t.Fatal(err)
	}
	srv.ask(t, ctx, target, `{"tool":"Bash","input":{},"nb":1}`)
	if _, err := srv.PromptSession(ctx, &agentfleetv1.PromptSessionRequest{CallerSessionId: caller, TargetSessionId: target, Text: "hi"}); err != nil {
		t.Errorf("prompting a session with an open decision: %v", err)
	}
}

// An older worker's unstamped permission holds its canUseTool promise, so
// that session really is blocked on a human and prompt_agent keeps
// refusing it (docs/adr/0082 §5). "nb":"1", a string, is no stamp.
func TestPromptSession_RefusesAnOlderWorkersBlockedSession(t *testing.T) {
	for _, req := range []string{`{"tool":"Bash","input":{}}`, `{"tool":"Bash","input":{},"nb":"1"}`} {
		ctx, srv, store, target, _ := nbServer(t)
		caller, err := store.Create(ctx, sessions.CreateParams{Repo: "agent-fleet"})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SetPodPhase(ctx, target, "POD_PHASE_RUNNING", ""); err != nil {
			t.Fatal(err)
		}
		srv.ask(t, ctx, target, req)
		if _, err := srv.PromptSession(ctx, &agentfleetv1.PromptSessionRequest{CallerSessionId: caller, TargetSessionId: target, Text: "hi"}); err == nil {
			t.Errorf("%s: prompting a blocked older worker was accepted", req)
		}
	}
}
