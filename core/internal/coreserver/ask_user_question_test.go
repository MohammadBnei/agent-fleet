package coreserver

import (
	"context"
	"sync"
	"testing"
	"time"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

// fakeQAStore is a plain in-memory transcript.Store — AskUserQuestion's
// matching logic is pure filtering over what ReadSince returns, not
// transcript.Store's own durability guarantees (covered by that package's
// own tests), so no real Postgres is needed here.
type fakeQAStore struct {
	mu      sync.Mutex
	nextSeq int64
	entries []transcript.Entry
}

func (f *fakeQAStore) Append(_ context.Context, _, from, text, msgType, _ string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	seq := f.nextSeq
	f.nextSeq++
	f.entries = append(f.entries, transcript.Entry{Seq: seq, From: from, Text: text, Type: msgType})
	return seq, nil
}

func (f *fakeQAStore) AppendReply(_ context.Context, _, from, text, msgType, _ string, replyToSeq int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	seq := f.nextSeq
	f.nextSeq++
	f.entries = append(f.entries, transcript.Entry{Seq: seq, From: from, Text: text, Type: msgType, ReplyTo: &replyToSeq})
	return seq, nil
}

func (f *fakeQAStore) ReadSince(_ context.Context, _ string, sinceSeq int64, _ int) ([]transcript.Entry, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []transcript.Entry
	next := sinceSeq
	for _, e := range f.entries {
		if e.Seq >= sinceSeq {
			out = append(out, e)
			next = e.Seq + 1
		}
	}
	return out, next, nil
}

func (f *fakeQAStore) LatestSeq(context.Context, string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nextSeq, nil
}

// TestAskUserQuestion_IgnoresAnswerToADifferentQuestion covers
// reliability-findings.md #0's real gap: before question-seq correlation,
// "any pending question + any reply" would let an unrelated answer (e.g.
// one replying to some other question, or a stale one) satisfy this call.
func TestAskUserQuestion_IgnoresAnswerToADifferentQuestion(t *testing.T) {
	store := &fakeQAStore{}
	s := New(store, nil, nil, nil, nil, nil, nil)

	// An answer to a *different* question (ReplyTo: 999) is already
	// sitting in the transcript before this call even posts its own
	// question — the old "any answer" check would have matched it.
	if _, err := store.AppendReply(context.Background(), "task-1", "human", "wrong answer", "answer", "", 999); err != nil {
		t.Fatalf("seed unrelated answer: %v", err)
	}

	req := &agentfleetv1.AskUserQuestionRequest{SessionId: "task-1", QuestionsJson: `{"questions":[]}`, TimeoutMs: 50}
	resp, err := s.AskUserQuestion(context.Background(), req)
	if err != nil {
		t.Fatalf("AskUserQuestion: %v", err)
	}
	if resp.GetAnswered() {
		t.Fatalf("expected answered=false (the unrelated answer must not satisfy this question), got %v", resp.GetAnswered())
	}
}

// TestAskUserQuestion_ReinvokeReusesSameQuestion covers the reported bug:
// on timeout the tool returns {"status":"pending"} and the agent re-invokes
// with the same questions. Each call used to Append a new question entry, so
// the human's answer (to the first seq) never matched the seq the re-invoke
// polled — answer lost forever. The re-invoke must reuse the first question's
// seq, and an answer to that seq must then satisfy a later call.
func TestAskUserQuestion_ReinvokeReusesSameQuestion(t *testing.T) {
	store := &fakeQAStore{}
	s := New(store, nil, nil, nil, nil, nil, nil)

	const questions = `{"questions":[{"question":"pick","header":"h","options":[]}]}`
	req := &agentfleetv1.AskUserQuestionRequest{SessionId: "task-1", QuestionsJson: questions, TimeoutMs: 30}

	first, err := s.AskUserQuestion(context.Background(), req)
	if err != nil {
		t.Fatalf("first AskUserQuestion: %v", err)
	}
	if first.GetAnswered() {
		t.Fatalf("expected first call to time out unanswered")
	}

	// Re-invoke with identical questions before anyone answers.
	second, err := s.AskUserQuestion(context.Background(), req)
	if err != nil {
		t.Fatalf("second AskUserQuestion: %v", err)
	}
	if second.GetQuestionSeq() != first.GetQuestionSeq() {
		t.Fatalf("re-invoke minted a new seq: first=%d second=%d", first.GetQuestionSeq(), second.GetQuestionSeq())
	}

	// Exactly one question entry exists — no duplicate cards.
	store.mu.Lock()
	questionCount := 0
	for _, e := range store.entries {
		if e.Type == "question" {
			questionCount++
		}
	}
	store.mu.Unlock()
	if questionCount != 1 {
		t.Fatalf("expected 1 question entry after re-invoke, got %d", questionCount)
	}

	// A human answer to the shared seq, landing while a re-invoke is still
	// blocked, unblocks it — the round trip that was silently lost before.
	go func() {
		time.Sleep(20 * time.Millisecond)
		_, _ = store.AppendReply(context.Background(), "task-1", "human", `{"answers":{"pick":"typed reply"}}`, "answer", "", first.GetQuestionSeq())
	}()
	waiting := &agentfleetv1.AskUserQuestionRequest{SessionId: "task-1", QuestionsJson: questions, TimeoutMs: 5000}
	third, err := s.AskUserQuestion(context.Background(), waiting)
	if err != nil {
		t.Fatalf("third AskUserQuestion: %v", err)
	}
	if !third.GetAnswered() || third.GetAnswersJson() != `{"answers":{"pick":"typed reply"}}` {
		t.Fatalf("answer to reused seq not received: answered=%v json=%q", third.GetAnswered(), third.GetAnswersJson())
	}
	if third.GetQuestionSeq() != first.GetQuestionSeq() {
		t.Fatalf("blocked call posted a new seq instead of reusing: first=%d third=%d", first.GetQuestionSeq(), third.GetQuestionSeq())
	}
}

// TestAskUserQuestion_ReaskAfterAnswerPostsFresh guards the regression the
// reuse logic must NOT introduce: once a question is answered, a later
// identical-text call is a genuine re-ask, so it must post a fresh question
// (new seq) and block — never silently return the prior answer.
func TestAskUserQuestion_ReaskAfterAnswerPostsFresh(t *testing.T) {
	store := &fakeQAStore{}
	s := New(store, nil, nil, nil, nil, nil, nil)

	const questions = `{"questions":[{"question":"proceed","header":"h","options":[]}]}`
	req := &agentfleetv1.AskUserQuestionRequest{SessionId: "task-1", QuestionsJson: questions, TimeoutMs: 30}

	// Answer the first ask (question is posted at seq 0).
	go func() {
		time.Sleep(10 * time.Millisecond)
		_, _ = store.AppendReply(context.Background(), "task-1", "human", `{"answers":{"proceed":"yes"}}`, "answer", "", 0)
	}()
	first := &agentfleetv1.AskUserQuestionRequest{SessionId: "task-1", QuestionsJson: questions, TimeoutMs: 5000}
	if _, err := s.AskUserQuestion(context.Background(), first); err != nil {
		t.Fatalf("first AskUserQuestion: %v", err)
	}

	// Re-ask identical text: must NOT return the stale answer, must post a
	// fresh question and time out unanswered.
	resp, err := s.AskUserQuestion(context.Background(), req)
	if err != nil {
		t.Fatalf("re-ask AskUserQuestion: %v", err)
	}
	if resp.GetAnswered() {
		t.Fatalf("re-ask returned a stale answer: %q", resp.GetAnswersJson())
	}
	if resp.GetQuestionSeq() == 0 {
		t.Fatalf("re-ask reused the answered seq 0 instead of posting fresh")
	}
	store.mu.Lock()
	questionCount := 0
	for _, e := range store.entries {
		if e.Type == "question" {
			questionCount++
		}
	}
	store.mu.Unlock()
	if questionCount != 2 {
		t.Fatalf("expected 2 question entries after a genuine re-ask, got %d", questionCount)
	}
}

// TestAskUserQuestion_MatchesCorrectlyTaggedAnswer is the positive case:
// an answer whose ReplyTo matches this call's own question seq does
// unblock it.
func TestAskUserQuestion_MatchesCorrectlyTaggedAnswer(t *testing.T) {
	store := &fakeQAStore{}
	s := New(store, nil, nil, nil, nil, nil, nil)

	go func() {
		// AskUserQuestion posts its own question first (seq 0, the only
		// prior entry) — give it a moment to do that before replying, so
		// this reply's ReplyTo:0 actually lines up with the real question
		// seq rather than racing it.
		time.Sleep(20 * time.Millisecond)
		_, _ = store.AppendReply(context.Background(), "task-1", "human", `{"answers":{}}`, "answer", "", 0)
	}()

	req := &agentfleetv1.AskUserQuestionRequest{SessionId: "task-1", QuestionsJson: `{"questions":[]}`, TimeoutMs: 5000}
	resp, err := s.AskUserQuestion(context.Background(), req)
	if err != nil {
		t.Fatalf("AskUserQuestion: %v", err)
	}
	if !resp.GetAnswered() {
		t.Fatalf("expected answered=true, got %v", resp.GetAnswered())
	}
	if resp.GetAnswersJson() != `{"answers":{}}` {
		t.Fatalf("unexpected answers_json: %q", resp.GetAnswersJson())
	}
}

// A re-ask whose text drifted must not leave the previous question pending.
//
// This is the failure that made questions unanswerable in production. Observed
// live 2026-08-19 on session b7753602, from core's own logs:
//
//	21:46:03  AskUserQuestion (3 questions)   -> discord: notified blocked
//	21:47:03  ERROR Canceled
//	21:47:15  AskUserQuestion (3 questions)   -> discord: notified blocked
//	21:48:15  ERROR Canceled
//	21:48:27  AnswerQuestion                  (the human answered)
//	21:49:37  discord: notified blocked       (again)
//
// announceBlocked only fires on a NEW append, so every retry was appending.
// Reuse compares question text byte-for-byte and the model regenerates that
// JSON each call, so it matched nothing. pending_decisions counts every
// unanswered question, so the session gathered one per retry and answering the
// visible card could never bring the count to zero — the human's report was
// "the question is dead, I can't even respond."
//
// Superseding rather than accumulating holds docs/adr/0018's invariant: only
// one AskUserQuestion is ever outstanding, because the agent's own tool call
// blocks.
func TestAskUserQuestion_ARewordedReAskSupersedesTheOldOne(t *testing.T) {
	store := &fakeQAStore{}
	s := New(store, nil, nil, nil, nil, nil, nil)
	ctx := context.Background()

	first := `{"questions":[{"header":"Storage","question":"which class?"}]}`
	// One word different, as a regenerated payload realistically is.
	second := `{"questions":[{"header":"Storage","question":"which storage class?"}]}`

	firstSeq, reused, err := s.reuseOrAppendQuestion(ctx, "task-1", first)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if reused {
		t.Fatal("the first question of a session cannot be a reuse")
	}

	secondSeq, reused, err := s.reuseOrAppendQuestion(ctx, "task-1", second)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if reused || secondSeq == firstSeq {
		t.Fatalf("reworded re-ask reused seq %d — the human would answer text the agent is no longer asking", firstSeq)
	}

	// Exactly one question may be outstanding.
	entries, _, err := store.ReadSince(ctx, "task-1", 0, 1000)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	answered := map[int64]bool{}
	for _, e := range entries {
		if e.Type == "answer" && e.ReplyTo != nil {
			answered[*e.ReplyTo] = true
		}
	}
	var pending []int64
	for _, e := range entries {
		if e.Type == "question" && !answered[e.Seq] {
			pending = append(pending, e.Seq)
		}
	}
	if len(pending) != 1 || pending[0] != secondSeq {
		t.Errorf("pending questions = %v, want exactly [%d] — every extra one is a decision the human can never clear", pending, secondSeq)
	}

	// The supersede must not look like a human answering, or core's poll would
	// hand it to the agent as the human's choice and the worker would replay it
	// as an input turn.
	for _, e := range entries {
		if e.Type == "answer" && e.From == "human" {
			t.Errorf("supersede was authored as human at seq %d — that resolves a human's decision on their behalf", e.Seq)
		}
	}
}
