package dashboard

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

// recordingStore captures the arguments its last Append call was made
// with — enough to verify Server.Kill/RespondToPermission call
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
	s := NewServer(nil, nil, store, nil, nil, nil, nil, nil, nil, 5, nil, nil, nil, "test")

	resp, err := s.RespondToPermission(context.Background(), connect.NewRequest(&agentfleetv1.RespondToPermissionRequest{
		SessionId: "task-1", Seq: 7, DecisionJson: `{"behavior":"allow"}`,
	}))
	if err != nil {
		t.Fatalf("RespondToPermission: %v", err)
	}
	if resp.Msg.GetSeq() < 0 {
		t.Errorf("status = %d, want %q", resp.Msg.GetSeq(), "answered")
	}
	if store.lastTaskID != "task-1" || store.lastFrom != "human" || store.lastText != `{"behavior":"allow"}` ||
		store.lastType != "permission_response" || store.lastReplyTo != 7 {
		t.Errorf("AppendReply(%q, %q, %q, %q, replyTo=%d), want (task-1, human, {\"behavior\":\"allow\"}, permission_response, replyTo=7)",
			store.lastTaskID, store.lastFrom, store.lastText, store.lastType, store.lastReplyTo)
	}
}

// What can and cannot be tested in this file, since it keeps coming up:
// `sessions.Store` and `repos.Store` are concrete Postgres-backed types, not
// interfaces a nil or fake can stand in for the way `recordingStore` does for
// `transcript.Store`. So anything reaching them — StopSession's
// MarkStopRequested, CreateSession's repo validation, PostMessage's WarmIfIdle
// — belongs in a `-tags=integration` test against a real database
// (`core/internal/dbtest`), and only the paths that return before touching a
// store can be unit-tested here.
//
// Three comments used to sit here naming kill_integration_test.go,
// create_task_integration_test.go and warm_integration_test.go as where those
// tests had moved. None of those files exists — they went with the dispatch
// and worktree machinery in docs/adr/0048, and the pointers outlived them.

func TestServer_Discuss_EmptyText(t *testing.T) {
	s := NewServer(nil, nil, &recordingStore{}, nil, nil, nil, nil, nil, nil, 5, nil, nil, nil, "test")

	req := connect.NewRequest(&agentfleetv1.PostMessageRequest{SessionId: "task-1", Text: ""})
	if _, err := s.PostMessage(context.Background(), req); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("Discuss error = %v, want CodeInvalidArgument", err)
	}
}

func TestServer_AnswerQuestion(t *testing.T) {
	store := &recordingStore{}
	s := NewServer(nil, nil, store, nil, nil, nil, nil, nil, nil, 5, nil, nil, nil, "test")

	answersJSON := `{"answers":{"Which quality attribute wins?":"Latency"}}`
	req := connect.NewRequest(&agentfleetv1.AnswerQuestionRequest{SessionId: "task-1", Seq: 3, AnswersJson: answersJSON})
	resp, err := s.AnswerQuestion(context.Background(), req)
	if err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}
	if resp.Msg.GetSeq() < 0 {
		t.Errorf("status = %d, want %q", resp.Msg.GetSeq(), "answered")
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
		{"interrupt", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_INTERRUPT},
		{"", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_UNSPECIFIED},
		{"garbage", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_UNSPECIFIED},
	}
	for _, c := range cases {
		if got := stringToProtoType(c.in); got != c.want {
			t.Errorf("stringToProtoType(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestValidModels_AreRealModelIDs guards an allowlist entry that was never a
// real model: `claude-haiku-4-5-20250929` — 20250929 is Sonnet 4.5's release
// date, pasted onto Haiku's name (Haiku 4.5 is 20251001).
//
// Nothing caught it because the allowlist is the only check, and it checks the
// string against itself: picking Haiku in the dashboard passed validation and
// then failed at the API, so the option had never worked in its entire life.
// A test asserting "the allowlist contains what the allowlist contains" would
// have passed too — the assertion has to be against the shape of a real ID.
//
// Dated snapshots are allowed (two legacy entries are still there), but a date
// is exactly the part that can be silently wrong, so new entries should be
// aliases.
func TestValidModels_AreRealModelIDs(t *testing.T) {
	// Snapshot dates that are known-good, so the check below can tell a real
	// dated ID from a transplanted one.
	knownDated := map[string]string{
		"claude-sonnet-4-5": "20250929",
		"claude-opus-4-5":   "20251101",
		"claude-haiku-4-5":  "20251001",
	}

	if validModels["claude-haiku-4-5-20250929"] {
		t.Error("claude-haiku-4-5-20250929 is back — that is Sonnet 4.5's date on Haiku's name; " +
			"it passes this allowlist and then 404s at the API")
	}
	if !validModels[defaultModel] {
		t.Errorf("defaultModel %q is not in validModels — the default would be rejected "+
			"if a caller ever passed it explicitly", defaultModel)
	}

	for model := range validModels {
		base, date, dated := strings.Cut(model, "-20")
		if !dated {
			continue // an alias — nothing to get wrong
		}
		want, known := knownDated[base]
		if !known {
			t.Errorf("%q is a dated snapshot of an unrecognized model %q — use an alias", model, base)
			continue
		}
		if got := "20" + date; got != want {
			t.Errorf("%q carries date %s, but %s was released %s — this is the "+
				"transplanted-date bug", model, got, base, want)
		}
	}
}
