package dashboard

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

// recordingStore captures the arguments its last Append call was made
// with — enough to verify Server.Approve/Stop call transcript.Store the
// same way core/internal/discord/handlers.go's Discord commands do
// (see docs/adr/0014, docs/adr/0015), without needing a real Postgres for
// logic this thin. transcript.Store.Append's own idempotency guarantee is
// PostgresStore's responsibility, not this package's. Server.GetE2EStatus/
// KillE2E aren't covered here — they're one-line pass-throughs to a
// concrete *provisionerclient.Client, already exercised by provisionerclient's own tests.
type recordingStore struct {
	lastTaskID, lastFrom, lastText, lastType string
	lastReplyTo                              int64
}

func (r *recordingStore) Append(_ context.Context, taskID, from, text, msgType, _ string) (int64, error) {
	r.lastTaskID, r.lastFrom, r.lastText, r.lastType = taskID, from, text, msgType
	return 0, nil
}

func (r *recordingStore) AppendReply(_ context.Context, taskID, from, text, msgType, _ string, replyToSeq int64) (int64, error) {
	r.lastTaskID, r.lastFrom, r.lastText, r.lastType, r.lastReplyTo = taskID, from, text, msgType, replyToSeq
	return 0, nil
}

func (r *recordingStore) ReadSince(context.Context, string, int64, int) ([]transcript.Entry, int64, error) {
	return nil, 0, nil
}

func TestServer_Approve(t *testing.T) {
	store := &recordingStore{}
	s := NewServer(nil, store, nil, nil, nil, nil)

	resp, err := s.Approve(context.Background(), connect.NewRequest(&agentfleetv1.ApproveRequest{TaskId: "task-1"}))
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if resp.Msg.GetStatus() != "approved" {
		t.Errorf("status = %q, want %q", resp.Msg.GetStatus(), "approved")
	}
	if store.lastTaskID != "task-1" || store.lastFrom != "human" || store.lastText != "approved" || store.lastType != "approve" {
		t.Errorf("Append(%q, %q, %q, %q), want (task-1, human, approved, approve)",
			store.lastTaskID, store.lastFrom, store.lastText, store.lastType)
	}
}

// Stop's own tests moved to stop_integration_test.go (build tag
// integration): Stop now also calls tasks.Store.MarkStopRequested (the
// grace-period-then-force-kill fix), and tasks.Store is a concrete
// Postgres-backed type here, not an interface a nil/fake can stand in for
// like recordingStore does for transcript.Store — same reasoning
// CreateTask's own comment below already gives for pass-through logic
// touching a real store.

// TestServer_CreateTask_UnknownRepo/EmptyDescription moved to
// create_task_integration_test.go — CreateTask now validates the repo via
// s.repos.Get (docs/adr/0028), a concrete Postgres-backed store, not a map
// lookup a nil store can stand in for (same reasoning stop_integration_
// test.go's own comment gives for Stop/tasks.Store).

// TestServer_Discuss covers the dashboard's free-text chat channel
// (reliability-findings.md's "seamless interaction" gap — the dashboard
// previously had no way to send arbitrary text, only structured
// Approve/Stop/AnswerQuestion) — full parity with a Discord reply: appended
// as a plain "discussion" entry, same as Approve/Stop hardcode their own type.
func TestServer_Discuss(t *testing.T) {
	store := &recordingStore{}
	s := NewServer(nil, store, nil, nil, nil, nil)

	req := connect.NewRequest(&agentfleetv1.DiscussRequest{TaskId: "task-1", Text: "what's the status?"})
	resp, err := s.Discuss(context.Background(), req)
	if err != nil {
		t.Fatalf("Discuss: %v", err)
	}
	if resp.Msg.GetStatus() != "sent" {
		t.Errorf("status = %q, want %q", resp.Msg.GetStatus(), "sent")
	}
	if store.lastTaskID != "task-1" || store.lastFrom != "human" || store.lastText != "what's the status?" || store.lastType != "discussion" {
		t.Errorf("Append(%q, %q, %q, %q), want (task-1, human, \"what's the status?\", discussion)",
			store.lastTaskID, store.lastFrom, store.lastText, store.lastType)
	}
}

func TestServer_Discuss_EmptyText(t *testing.T) {
	s := NewServer(nil, &recordingStore{}, nil, nil, nil, nil)

	req := connect.NewRequest(&agentfleetv1.DiscussRequest{TaskId: "task-1", Text: ""})
	if _, err := s.Discuss(context.Background(), req); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("Discuss error = %v, want CodeInvalidArgument", err)
	}
}

func TestServer_AnswerQuestion(t *testing.T) {
	store := &recordingStore{}
	s := NewServer(nil, store, nil, nil, nil, nil)

	answersJSON := `{"answers":{"Which quality attribute wins?":"Latency"}}`
	req := connect.NewRequest(&agentfleetv1.AnswerQuestionRequest{TaskId: "task-1", Seq: 3, AnswersJson: answersJSON})
	resp, err := s.AnswerQuestion(context.Background(), req)
	if err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}
	if resp.Msg.GetStatus() != "answered" {
		t.Errorf("status = %q, want %q", resp.Msg.GetStatus(), "answered")
	}
	if store.lastTaskID != "task-1" || store.lastFrom != "human" || store.lastText != answersJSON || store.lastType != "answer" {
		t.Errorf("AppendReply(%q, %q, %q, %q), want (task-1, human, %s, answer)",
			store.lastTaskID, store.lastFrom, store.lastText, store.lastType, answersJSON)
	}
	if store.lastReplyTo != 3 {
		t.Errorf("lastReplyTo = %d, want 3 (the question's own seq, reliability-findings.md #0's correlation)", store.lastReplyTo)
	}
}

// TestStringToProtoType covers the read-path mapping the dashboard's
// GetTranscript/StreamTranscript rely on — "tool_call" was accepted by the
// DB CHECK constraint and by coreserver's own ingestion-path copy of this
// switch, but silently fell through to UNSPECIFIED here, making
// sidecar-pushed tool telemetry indistinguishable from a plain message in
// the dashboard UI.
func TestStringToProtoType(t *testing.T) {
	cases := []struct {
		in   string
		want agentfleetv1.TranscriptEntryType
	}{
		{"discussion", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_DISCUSSION},
		{"approve", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_APPROVE},
		{"abort", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_ABORT},
		{"question", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_QUESTION},
		{"answer", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_ANSWER},
		{"tool_call", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_TOOL_CALL},
		{"system", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_SYSTEM},
		{"assistant", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_ASSISTANT},
		{"user", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_USER},
		{"result", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_RESULT},
		{"", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_UNSPECIFIED},
		{"garbage", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_UNSPECIFIED},
	}
	for _, c := range cases {
		if got := stringToProtoType(c.in); got != c.want {
			t.Errorf("stringToProtoType(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
