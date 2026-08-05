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

// TestAskUserQuestion_IgnoresAnswerToADifferentQuestion covers
// reliability-findings.md #0's real gap: before question-seq correlation,
// "any pending question + any reply" would let an unrelated answer (e.g.
// one replying to some other question, or a stale one) satisfy this call.
func TestAskUserQuestion_IgnoresAnswerToADifferentQuestion(t *testing.T) {
	store := &fakeQAStore{}
	s := New(store, nil, nil, nil)

	// An answer to a *different* question (ReplyTo: 999) is already
	// sitting in the transcript before this call even posts its own
	// question — the old "any answer" check would have matched it.
	if _, err := store.AppendReply(context.Background(), "task-1", "human", "wrong answer", "answer", "", 999); err != nil {
		t.Fatalf("seed unrelated answer: %v", err)
	}

	req := &agentfleetv1.AskUserQuestionRequest{TaskId: "task-1", QuestionsJson: `{"questions":[]}`, TimeoutMs: 50}
	resp, err := s.AskUserQuestion(context.Background(), req)
	if err != nil {
		t.Fatalf("AskUserQuestion: %v", err)
	}
	if resp.GetStatus() != "pending" {
		t.Fatalf("expected status=pending (the unrelated answer must not satisfy this question), got %q", resp.GetStatus())
	}
}

// TestAskUserQuestion_MatchesCorrectlyTaggedAnswer is the positive case:
// an answer whose ReplyTo matches this call's own question seq does
// unblock it.
func TestAskUserQuestion_MatchesCorrectlyTaggedAnswer(t *testing.T) {
	store := &fakeQAStore{}
	s := New(store, nil, nil, nil)

	go func() {
		// AskUserQuestion posts its own question first (seq 0, the only
		// prior entry) — give it a moment to do that before replying, so
		// this reply's ReplyTo:0 actually lines up with the real question
		// seq rather than racing it.
		time.Sleep(20 * time.Millisecond)
		_, _ = store.AppendReply(context.Background(), "task-1", "human", `{"answers":{}}`, "answer", "", 0)
	}()

	req := &agentfleetv1.AskUserQuestionRequest{TaskId: "task-1", QuestionsJson: `{"questions":[]}`, TimeoutMs: 5000}
	resp, err := s.AskUserQuestion(context.Background(), req)
	if err != nil {
		t.Fatalf("AskUserQuestion: %v", err)
	}
	if resp.GetStatus() != "answered" {
		t.Fatalf("expected status=answered, got %q", resp.GetStatus())
	}
	if resp.GetAnswersJson() != `{"answers":{}}` {
		t.Fatalf("unexpected answers_json: %q", resp.GetAnswersJson())
	}
}
