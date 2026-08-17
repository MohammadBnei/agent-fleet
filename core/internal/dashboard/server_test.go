package dashboard

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

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

// The dashboard's plan-approval path sets "auto" (docs/adr/0052), and this
// allowlist is the only thing between that request and the SDK — an omission
// here surfaces as InvalidArgument on a button, not as a compile error.
func TestValidPermissionModes_CoversEveryModeTheDashboardCanSend(t *testing.T) {
	for _, mode := range []string{"default", "plan", "acceptEdits", "auto", "dontAsk", "bypassPermissions"} {
		if !validPermissionModes[mode] {
			t.Errorf("validPermissionModes[%q] = false, want true", mode)
		}
	}
	if validPermissionModes["delegate"] {
		t.Error(`validPermissionModes["delegate"] = true, want false (not a mode this build accepts)`)
	}
}

// The transcript window is the fix for a silent truncation: GetTranscript
// always read forward from since_seq with a hard cap of 1000, so a session
// past 1000 entries handed the dashboard its OLDEST page and left everything
// between there and the live stream's start unfetched by anything. The detail
// view now opens on the newest page and walks backwards.
func TestTranscriptWindow(t *testing.T) {
	before := func(v int64) *int64 { return &v }
	for _, tc := range []struct {
		name      string
		sinceSeq  int64
		latestSeq int64
		limit     int32
		beforeSeq *int64
		wantSince int64
		wantLimit int
	}{
		// Neither field set: exactly what the sidecar's cursor reads have
		// always done. Adding the window must not move this.
		{"no limit, no before: unchanged forward read", 0, 0, 0, nil, 0, maxTranscriptPage},
		{"cursor read keeps its since_seq", 4200, 0, 0, nil, 4200, maxTranscriptPage},

		// The newest page: latestSeq is one past the highest seq.
		{"newest page of a long transcript", 0, 5000, 200, nil, 4800, 200},
		{"transcript shorter than a page starts at 0", 0, 12, 200, nil, 0, 200},
		{"empty transcript", 0, 0, 200, nil, 0, 200},
		// A caller that supplies both gets the tail of its own range, never
		// entries below the cursor it already holds.
		{"since_seq floors the newest page", 4900, 5000, 200, nil, 4900, 200},

		// Walking backwards.
		{"page before a seq", 0, 5000, 200, before(4800), 4600, 200},
		{"page before the start clamps at 0", 0, 5000, 200, before(80), 0, 200},

		// The cap is a ceiling on the response, not a value a client can raise.
		{"oversized limit clamps to the cap", 0, 9000, 50_000, nil, 8000, maxTranscriptPage},
		{"negative limit falls back to the cap", 0, 9000, -3, nil, 0, maxTranscriptPage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			since, limit := transcriptWindow(tc.sinceSeq, tc.latestSeq, tc.limit, tc.beforeSeq)
			if since != tc.wantSince || limit != tc.wantLimit {
				t.Errorf("transcriptWindow(%d, %d, %d, %v) = (%d, %d), want (%d, %d)",
					tc.sinceSeq, tc.latestSeq, tc.limit, tc.beforeSeq, since, limit, tc.wantSince, tc.wantLimit)
			}
		})
	}
}

// denseStore serves a transcript of `total` entries with contiguous seqs,
// the way Append actually assigns them.
type denseStore struct {
	recordingStore
	total int64
}

func (d *denseStore) LatestSeq(context.Context, string) (int64, error) { return d.total, nil }

func (d *denseStore) ReadSince(_ context.Context, _ string, sinceSeq int64, limit int) ([]transcript.Entry, int64, error) {
	entries := []transcript.Entry{}
	for seq := sinceSeq; seq < d.total && len(entries) < limit; seq++ {
		entries = append(entries, transcript.Entry{Seq: seq, From: "agent", Text: "x"})
	}
	next := sinceSeq
	if n := len(entries); n > 0 {
		next = entries[n-1].Seq + 1
	}
	return entries, next, nil
}

func TestGetTranscript_OpensOnTheNewestPageAndWalksBack(t *testing.T) {
	store := &denseStore{total: 1200}
	s := NewServer(nil, nil, store, nil, nil, nil, nil, nil, nil, 5, nil, nil, nil, "test")

	// The regression: with limit set, the newest entries come back — not
	// seq 0..999.
	resp, err := s.GetTranscript(context.Background(), connect.NewRequest(&agentfleetv1.ReadTranscriptSinceRequest{
		SessionId: "s1", Limit: proto.Int32(200),
	}))
	if err != nil {
		t.Fatalf("GetTranscript: %v", err)
	}
	got := resp.Msg.GetEntries()
	if len(got) != 200 || got[0].GetSeq() != 1000 || got[199].GetSeq() != 1199 {
		t.Fatalf("newest page = %d entries [%d..%d], want 200 [1000..1199]",
			len(got), got[0].GetSeq(), got[len(got)-1].GetSeq())
	}
	// next_seq must still point past the newest entry, or the live stream
	// would resubscribe into history it already has.
	if resp.Msg.GetNextSeq() != 1200 {
		t.Errorf("next_seq = %d, want 1200", resp.Msg.GetNextSeq())
	}

	// One click of "load earlier": the page immediately before what's held,
	// and nothing at or past the boundary (which would duplicate on prepend).
	older, err := s.GetTranscript(context.Background(), connect.NewRequest(&agentfleetv1.ReadTranscriptSinceRequest{
		SessionId: "s1", Limit: proto.Int32(200), BeforeSeq: proto.Int64(1000),
	}))
	if err != nil {
		t.Fatalf("GetTranscript(before): %v", err)
	}
	got = older.Msg.GetEntries()
	if len(got) != 200 || got[0].GetSeq() != 800 || got[199].GetSeq() != 999 {
		t.Fatalf("older page = %d entries [%d..%d], want 200 [800..999]",
			len(got), got[0].GetSeq(), got[len(got)-1].GetSeq())
	}
}
