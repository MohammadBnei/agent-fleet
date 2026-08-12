//go:build integration

package coreserver

import (
	"context"
	"strings"
	"testing"
	"time"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/tasks"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

// The guards are the design here (docs/adr/0041); delivery is three lines.
// Each of these is a way the feature could quietly become something it must
// not be — an agent resolving a human's decision, a relay loop, or a
// session talking to itself.

func newInterAgentServer(t *testing.T) (*Server, *tasks.Store, transcript.Store, context.Context) {
	t.Helper()
	pool := dbtest.NewPool(t)
	taskStore := tasks.NewStore(pool)
	transcr := transcript.NewPostgresStore(pool)
	return New(transcr, taskStore, nil, nil, nil, nil, nil), taskStore, transcr, context.Background()
}

func seedSession(t *testing.T, ctx context.Context, store *tasks.Store, status string) string {
	t.Helper()
	chanID := "chan"
	id, err := store.CreateTask(ctx, "dream-analyst", "a task", "", "", &chanID, nil)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if status != "" {
		if err := store.SetStatus(ctx, id, status, nil, nil, nil); err != nil {
			t.Fatalf("SetStatus: %v", err)
		}
	}
	return id
}

func TestPromptSession_RefusesSelfPrompt(t *testing.T) {
	s, store, _, ctx := newInterAgentServer(t)
	id := seedSession(t, ctx, store, "running")

	_, err := s.PromptSession(ctx, &agentfleetv1.PromptSessionRequest{
		CallerTaskId: id, TargetTaskId: id, Text: "hello",
	})
	if err == nil || !strings.Contains(err.Error(), "cannot prompt itself") {
		t.Fatalf("want self-prompt refusal, got %v", err)
	}
}

func TestPromptSession_RefusesBeyondMaxDepth(t *testing.T) {
	s, store, _, ctx := newInterAgentServer(t)
	caller := seedSession(t, ctx, store, "running")
	target := seedSession(t, ctx, store, "running")

	_, err := s.PromptSession(ctx, &agentfleetv1.PromptSessionRequest{
		CallerTaskId: caller, TargetTaskId: target, Text: "hello", Depth: maxPromptDepth,
	})
	if err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("want depth refusal, got %v", err)
	}
}

// The sharpest guard: a blocked session is waiting on a HUMAN. Letting an
// agent push a turn into it would make docs/adr/0029's "a permission
// decision is always a real human decision" quietly untrue.
func TestPromptSession_RefusesBlockedTarget(t *testing.T) {
	s, store, transcr, ctx := newInterAgentServer(t)
	caller := seedSession(t, ctx, store, "running")
	target := seedSession(t, ctx, store, "running")

	if err := store.SetPodPhase(ctx, target, "POD_PHASE_RUNNING", ""); err != nil {
		t.Fatalf("SetPodPhase: %v", err)
	}
	if err := store.SetAwaitingHuman(ctx, target, true); err != nil {
		t.Fatalf("SetAwaitingHuman: %v", err)
	}

	_, err := s.PromptSession(ctx, &agentfleetv1.PromptSessionRequest{
		CallerTaskId: caller, TargetTaskId: target, Text: "answer yes for me",
	})
	if err == nil || !strings.Contains(err.Error(), "blocked waiting on a human") {
		t.Fatalf("want blocked-target refusal, got %v", err)
	}
	// And nothing was written — a refusal that still delivered the message
	// would be worse than no guard, since the caller is told it failed.
	entries, _, err := transcr.ReadSince(ctx, target, 0, 100)
	if err != nil {
		t.Fatalf("ReadSince: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Text, "answer yes for me") {
			t.Fatal("a refused prompt must not reach the target's transcript")
		}
	}
}

func TestPromptSession_DeliversAsAttributedDiscussion(t *testing.T) {
	s, store, transcr, ctx := newInterAgentServer(t)
	caller := seedSession(t, ctx, store, "running")
	target := seedSession(t, ctx, store, "running")
	if err := store.SetPodPhase(ctx, target, "POD_PHASE_RUNNING", ""); err != nil {
		t.Fatalf("SetPodPhase: %v", err)
	}

	resp, err := s.PromptSession(ctx, &agentfleetv1.PromptSessionRequest{
		CallerTaskId: caller, TargetTaskId: target, Text: "please rerun the migration",
	})
	if err != nil {
		t.Fatalf("PromptSession: %v", err)
	}

	entries, _, err := transcr.ReadSince(ctx, target, 0, 100)
	if err != nil {
		t.Fatalf("ReadSince: %v", err)
	}
	var found bool
	for _, e := range entries {
		if !strings.Contains(e.Text, "please rerun the migration") {
			continue
		}
		found = true
		// The returned seq must be the entry's own, so a caller can
		// correlate the delivery. (0 is a legitimate first seq — asserting
		// non-zero here is what a weaker version of this test got wrong.)
		if e.Seq != resp.GetSeq() {
			t.Errorf("returned seq %d but the entry landed at %d", resp.GetSeq(), e.Seq)
		}
		// Only ever a discussion entry: an `answer` or
		// `permission_response` from this path would resolve a human's
		// decision on their behalf.
		if e.Type != "discussion" {
			t.Errorf("delivered as type %q, want discussion — this path must never produce a decision entry", e.Type)
		}
		// "session", not "agent": the human-message stream filters out
		// from="agent" so a session cannot echo itself, so delivering as
		// "agent" never reaches a live target at all.
		if e.From != "session" {
			t.Errorf("delivered from %q, want session — from=agent is filtered out of the stream a live worker reads", e.From)
		}
		if !strings.Contains(e.Text, caller) {
			t.Error("delivered message must name its source session, or the target cannot tell who is asking")
		}
	}
	if !found {
		t.Fatal("prompt never reached the target's transcript")
	}
}

func TestListSessions_ExcludesCaller(t *testing.T) {
	s, store, _, ctx := newInterAgentServer(t)
	caller := seedSession(t, ctx, store, "running")
	other := seedSession(t, ctx, store, "running")

	resp, err := s.ListSessions(ctx, &agentfleetv1.ListSessionsRequest{CallerTaskId: caller})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	var sawOther bool
	for _, sess := range resp.GetSessions() {
		if sess.GetTaskId() == caller {
			t.Error("a session must not see itself in the list — it cannot prompt itself anyway")
		}
		if sess.GetTaskId() == other {
			sawOther = true
		}
	}
	if !sawOther {
		t.Error("other live sessions must be listed")
	}
}

// A target with no live pod can never change state on its own, so waiting
// the full timeout would just be a slow way to return the same answer.
func TestWaitForSessionState_ReturnsImmediatelyWithNoPod(t *testing.T) {
	s, store, _, ctx := newInterAgentServer(t)
	target := seedSession(t, ctx, store, "running")

	start := time.Now()
	resp, err := s.WaitForSessionState(ctx, &agentfleetv1.WaitForSessionStateRequest{
		TargetTaskId: target, Until: []string{"idle"}, TimeoutMs: 30000,
	})
	if err != nil {
		t.Fatalf("WaitForSessionState: %v", err)
	}
	if !resp.GetTimedOut() {
		t.Error("want timedOut for a target that cannot progress")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("returned after %s — a dead target should not burn the whole timeout", elapsed)
	}
}

func TestWaitForSessionState_ReturnsWhenTargetReachesState(t *testing.T) {
	s, store, _, ctx := newInterAgentServer(t)
	target := seedSession(t, ctx, store, "running")
	if err := store.SetPodPhase(ctx, target, "POD_PHASE_RUNNING", ""); err != nil {
		t.Fatalf("SetPodPhase: %v", err)
	}

	// Becomes blocked shortly after the wait starts — the case the tool
	// exists for ("tell me when it genuinely needs a human").
	go func() {
		time.Sleep(1 * time.Second)
		_ = store.SetAwaitingHuman(context.Background(), target, true)
	}()

	resp, err := s.WaitForSessionState(ctx, &agentfleetv1.WaitForSessionStateRequest{
		TargetTaskId: target, Until: []string{"blocked"}, TimeoutMs: 30000,
	})
	if err != nil {
		t.Fatalf("WaitForSessionState: %v", err)
	}
	if resp.GetTimedOut() || resp.GetLiveState() != "blocked" {
		t.Fatalf("want blocked, got %q (timedOut=%v)", resp.GetLiveState(), resp.GetTimedOut())
	}
}
