package mcpserver

import (
	"context"
	"errors"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

type recordedEntry struct {
	from       string
	text       string
	replyToSeq int64
	isReply    bool
}

type fakeRecorder struct {
	entries  []recordedEntry
	nextSeq  int64
	failSend bool
}

func (f *fakeRecorder) SendMessage(_ context.Context, from, text string, _ agentfleetv1.TranscriptEntryType, _ string) (int64, error) {
	if f.failSend {
		return 0, errors.New("core unreachable")
	}
	f.nextSeq++
	f.entries = append(f.entries, recordedEntry{from: from, text: text})
	return f.nextSeq, nil
}

func (f *fakeRecorder) AppendReplyMessage(_ context.Context, from, text string, _ agentfleetv1.TranscriptEntryType, _ string, replyToSeq int64) (int64, error) {
	f.nextSeq++
	f.entries = append(f.entries, recordedEntry{from: from, text: text, replyToSeq: replyToSeq, isReply: true})
	return f.nextSeq, nil
}

type fakeAsker struct {
	answer string
	err    error
	asked  string
}

func (f *fakeAsker) Ask(_ context.Context, question string) (string, error) {
	f.asked = question
	return f.answer, f.err
}

func callAskThot(t *testing.T, rec *fakeRecorder, asker *fakeAsker, question string) (*mcp.CallToolResult, error) {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name = "ask_thot"
	req.Params.Arguments = map[string]any{"question": question}
	return askThotHandler(rec, asker)(context.Background(), req)
}

// docs/adr/0035's audit-trail constraint: a direct sidecar→thot call must
// still land in the asking task's own transcript, correlated. This is the
// test that fails if that double-write is ever dropped as "redundant".
func TestAskThotRecordsBothSidesCorrelated(t *testing.T) {
	rec := &fakeRecorder{}
	asker := &fakeAsker{answer: "the pod is OOMKilled"}

	res, err := callAskThot(t, rec, asker, "why is my pod failing?")
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res)
	}

	if len(rec.entries) != 2 {
		t.Fatalf("expected question + answer recorded, got %d entries: %+v", len(rec.entries), rec.entries)
	}
	q, a := rec.entries[0], rec.entries[1]
	if q.from != "agent" {
		t.Errorf("question should be attributed to the agent, got %q", q.from)
	}
	if a.from != "thot" {
		t.Errorf("answer should be attributed to thot, got %q", a.from)
	}
	if !a.isReply {
		t.Error("answer must be recorded as a reply, not a bare append")
	}
	if a.replyToSeq != 1 {
		t.Errorf("answer should correlate to the question's seq 1, got %d", a.replyToSeq)
	}
	if asker.asked != "why is my pod failing?" {
		t.Errorf("thot got the wrong question: %q", asker.asked)
	}
}

// If the question can't be recorded, the call must not silently proceed —
// that would produce an answer the task's audit trail never shows.
func TestAskThotFailsWhenQuestionCannotBeRecorded(t *testing.T) {
	rec := &fakeRecorder{failSend: true}
	asker := &fakeAsker{answer: "should never be reached"}

	if _, err := callAskThot(t, rec, asker, "anything"); err == nil {
		t.Fatal("expected an error when the transcript write fails")
	}
	if asker.asked != "" {
		t.Error("thot must not be asked when the question could not be recorded")
	}
}

// An unreachable thot is a tool-level error the agent can react to, not a
// handler crash — and it must not be reported as an answer.
func TestAskThotSurfacesThotFailure(t *testing.T) {
	rec := &fakeRecorder{}
	asker := &fakeAsker{err: errors.New("connection refused")}

	res, err := callAskThot(t, rec, asker, "why?")
	if err != nil {
		t.Fatalf("expected a tool-level error result, got a handler error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError when thot is unreachable")
	}
	// The question is still on the record even though no answer came back.
	if len(rec.entries) != 1 {
		t.Fatalf("expected only the question recorded, got %+v", rec.entries)
	}
}
