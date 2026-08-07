package discord

import (
	"context"
	"strings"
	"testing"

	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

// fakeTranscriptStore backs findPendingQuestionSeq's own tests — a plain
// in-memory slice is enough, since the logic under test is pure filtering/
// scanning, not transcript.Store's own durability guarantees (already
// that store's responsibility, covered by its own package's tests).
type fakeTranscriptStore struct {
	entries []transcript.Entry
}

func (f *fakeTranscriptStore) Append(context.Context, string, string, string, string, string) (int64, error) {
	panic("not used by findPendingQuestionSeq")
}

func (f *fakeTranscriptStore) AppendReply(context.Context, string, string, string, string, string, int64) (int64, error) {
	panic("not used by findPendingQuestionSeq")
}

func (f *fakeTranscriptStore) ReadSince(_ context.Context, _ string, sinceSeq int64, _ int) ([]transcript.Entry, int64, error) {
	var out []transcript.Entry
	for _, e := range f.entries {
		if e.Seq >= sinceSeq {
			out = append(out, e)
		}
	}
	return out, 0, nil
}

func (f *fakeTranscriptStore) LatestSeq(context.Context, string) (int64, error) {
	panic("not used by findPendingQuestionSeq")
}

func int64Ptr(i int64) *int64 { return &i }

// TestFindPendingQuestionSeq_ReturnsMostRecentUnansweredQuestion and its
// sibling below cover reliability-findings.md #0: before this, Discord
// had no way to answer a question at all (every free-text reply defaulted
// to "discussion") — findPendingQuestionSeq is what lets onMessageCreate
// route a reply to the right pending question via ReplyTo correlation,
// the same one AskUserQuestion's own matching loop
// (coreserver.Server.AskUserQuestion) uses.
func TestFindPendingQuestionSeq_ReturnsMostRecentUnansweredQuestion(t *testing.T) {
	store := &fakeTranscriptStore{entries: []transcript.Entry{
		{Seq: 1, From: "agent", Type: "question"},
		{Seq: 2, From: "human", Type: "answer", ReplyTo: int64Ptr(1)},
		{Seq: 3, From: "agent", Type: "question"},
	}}
	c := &Client{transcr: store}

	seq, err := c.findPendingQuestionSeq(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("findPendingQuestionSeq: %v", err)
	}
	if seq != 3 {
		t.Fatalf("expected the most recent unanswered question (seq 3), got %d", seq)
	}
}

func TestFindPendingQuestionSeq_ReturnsZeroWhenAllAnswered(t *testing.T) {
	store := &fakeTranscriptStore{entries: []transcript.Entry{
		{Seq: 1, From: "agent", Type: "question"},
		{Seq: 2, From: "human", Type: "answer", ReplyTo: int64Ptr(1)},
	}}
	c := &Client{transcr: store}

	seq, err := c.findPendingQuestionSeq(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("findPendingQuestionSeq: %v", err)
	}
	if seq != 0 {
		t.Fatalf("expected 0 (no pending question), got %d", seq)
	}
}

func TestFindPendingQuestionSeq_ReturnsZeroWhenNoQuestionsAtAll(t *testing.T) {
	store := &fakeTranscriptStore{entries: []transcript.Entry{
		{Seq: 1, From: "human", Type: "discussion"},
	}}
	c := &Client{transcr: store}

	seq, err := c.findPendingQuestionSeq(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("findPendingQuestionSeq: %v", err)
	}
	if seq != 0 {
		t.Fatalf("expected 0, got %d", seq)
	}
}

func TestThreadName_TruncatesLongDescription(t *testing.T) {
	long := strings.Repeat("a", 200)
	got := threadName("dream-analyst", long)

	if len(got) > 100 {
		t.Fatalf("thread name is %d runes, want <= 100 (Discord's limit): %q", len([]rune(got)), got)
	}
	if !strings.HasPrefix(got, "dream-analyst: aaa") {
		t.Fatalf("got %q, want it to start with repo prefix", got)
	}
}

func TestThreadName_KeepsShortDescriptionIntact(t *testing.T) {
	got := threadName("vos-monolith", "fix the login bug")
	want := "vos-monolith: fix the login bug"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
