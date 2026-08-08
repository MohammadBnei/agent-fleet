package dashboard

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

// recordingStore captures the arguments its last Append call was made
// with — enough to verify Server.Stop/RespondToPermission call
// transcript.Store the same way core/internal/discord/handlers.go's
// Discord commands do
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

func (r *recordingStore) LatestSeq(context.Context, string) (int64, error) {
	return 0, nil
}

func TestServer_RespondToPermission(t *testing.T) {
	store := &recordingStore{}
	s := NewServer(nil, store, nil, nil, nil, nil, nil, nil, 5)

	resp, err := s.RespondToPermission(context.Background(), connect.NewRequest(&agentfleetv1.RespondToPermissionRequest{
		TaskId: "task-1", Seq: 7, DecisionJson: `{"behavior":"allow"}`,
	}))
	if err != nil {
		t.Fatalf("RespondToPermission: %v", err)
	}
	if resp.Msg.GetStatus() != "answered" {
		t.Errorf("status = %q, want %q", resp.Msg.GetStatus(), "answered")
	}
	if store.lastTaskID != "task-1" || store.lastFrom != "human" || store.lastText != `{"behavior":"allow"}` ||
		store.lastType != "permission_response" || store.lastReplyTo != 7 {
		t.Errorf("AppendReply(%q, %q, %q, %q, replyTo=%d), want (task-1, human, {\"behavior\":\"allow\"}, permission_response, replyTo=7)",
			store.lastTaskID, store.lastFrom, store.lastText, store.lastType, store.lastReplyTo)
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

// TestServer_Discuss's happy-path coverage moved to
// warm_integration_test.go (TestServer_Discuss_LivePod_NoWarmAttempt) —
// Discuss now also calls warmIfIdle, which touches tasks.Store (concrete,
// not an interface a nil store can stand in for), same reasoning
// stop_integration_test.go's own comment gives for Stop. EmptyText stays
// here since it returns before ever reaching tasks.Store.

func TestServer_Discuss_EmptyText(t *testing.T) {
	s := NewServer(nil, &recordingStore{}, nil, nil, nil, nil, nil, nil, 5)

	req := connect.NewRequest(&agentfleetv1.DiscussRequest{TaskId: "task-1", Text: ""})
	if _, err := s.Discuss(context.Background(), req); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("Discuss error = %v, want CodeInvalidArgument", err)
	}
}

func TestServer_AnswerQuestion(t *testing.T) {
	store := &recordingStore{}
	s := NewServer(nil, store, nil, nil, nil, nil, nil, nil, 5)

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
		{"permission_mode", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_PERMISSION_MODE},
		{"permission_request", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_PERMISSION_REQUEST},
		{"permission_response", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_PERMISSION_RESPONSE},
		{"", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_UNSPECIFIED},
		{"garbage", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_UNSPECIFIED},
	}
	for _, c := range cases {
		if got := stringToProtoType(c.in); got != c.want {
			t.Errorf("stringToProtoType(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
