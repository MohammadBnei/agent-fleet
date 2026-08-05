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
}

func (r *recordingStore) Append(_ context.Context, taskID, from, text, msgType, _ string) (int64, error) {
	r.lastTaskID, r.lastFrom, r.lastText, r.lastType = taskID, from, text, msgType
	return 0, nil
}

func (r *recordingStore) ReadSince(context.Context, string, int64, int) ([]transcript.Entry, int64, error) {
	return nil, 0, nil
}

func TestServer_Approve(t *testing.T) {
	store := &recordingStore{}
	s := NewServer(nil, store, nil, nil, nil)

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

func TestServer_Stop_DefaultReason(t *testing.T) {
	store := &recordingStore{}
	s := NewServer(nil, store, nil, nil, nil)

	resp, err := s.Stop(context.Background(), connect.NewRequest(&agentfleetv1.StopRequest{TaskId: "task-1"}))
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if resp.Msg.GetStatus() != "stopping" {
		t.Errorf("status = %q, want %q", resp.Msg.GetStatus(), "stopping")
	}
	if store.lastText != "stopped by human" || store.lastType != "abort" {
		t.Errorf("got (%q, %q), want (stopped by human, abort)", store.lastText, store.lastType)
	}
}

func TestServer_Stop_CustomReason(t *testing.T) {
	store := &recordingStore{}
	s := NewServer(nil, store, nil, nil, nil)

	reason := "wrong direction"
	req := connect.NewRequest(&agentfleetv1.StopRequest{TaskId: "task-1", Reason: &reason})
	if _, err := s.Stop(context.Background(), req); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if store.lastText != "wrong direction" {
		t.Errorf("text = %q, want %q", store.lastText, "wrong direction")
	}
}

// TestServer_CreateTask_UnknownRepo/EmptyDescription cover the two
// validation branches that return before ever touching tasks.Store — hence
// the nil taskStore, same pattern as the other tests in this file passing
// nil for stores CreateTask/Approve/Stop don't need. The success path
// (a real INSERT with nil channel/thread) is exercised by tasks package's
// own TestCreateTask_NilChannelAndThread — this handler is a one-line
// pass-through to that store method plus taskToProto, not new logic worth
// its own testcontainers setup (see this file's own comment on
// GetE2EStatus/KillE2E for the same reasoning).
func TestServer_CreateTask_UnknownRepo(t *testing.T) {
	s := NewServer(nil, nil, nil, nil, nil)

	req := connect.NewRequest(&agentfleetv1.CreateTaskRequest{Repo: "not-a-real-repo", Description: "do something"})
	if _, err := s.CreateTask(context.Background(), req); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("CreateTask error = %v, want CodeInvalidArgument", err)
	}
}

func TestServer_CreateTask_EmptyDescription(t *testing.T) {
	s := NewServer(nil, nil, nil, nil, nil)

	req := connect.NewRequest(&agentfleetv1.CreateTaskRequest{Repo: "dream-analyst", Description: ""})
	if _, err := s.CreateTask(context.Background(), req); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("CreateTask error = %v, want CodeInvalidArgument", err)
	}
}

func TestServer_AnswerQuestion(t *testing.T) {
	store := &recordingStore{}
	s := NewServer(nil, store, nil, nil, nil)

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
		t.Errorf("Append(%q, %q, %q, %q), want (task-1, human, %s, answer)",
			store.lastTaskID, store.lastFrom, store.lastText, store.lastType, answersJSON)
	}
}
