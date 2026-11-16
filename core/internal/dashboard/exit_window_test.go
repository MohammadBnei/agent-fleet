//go:build integration

package dashboard

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// docs/adr/0070 §6, the exit window: a single-shot pod writes its exit result
// and exits, but pod_phase still reads live, so an answer in that gap warms
// nothing and lands above the leaving pod's consumedSeq. Once the pod is gone
// every warm (a sender's, or sessions.Loop's endRuns when nobody sends)
// starts the next pod just past that consumedSeq, so the answer reaches it.
// A warm at LatestSeq would start above it: never delivered.
func TestExitWindow_AnswerReachesTheNextPod(t *testing.T) {
	ctx := context.Background()
	s, prov, _, store, pool := newDefinitionsServerPool(t)
	s.decisions = decisions.NewStore(pool)
	sess, err := createDef(s, "dev-dream-analyst", nil)
	if err != nil {
		t.Fatal(err)
	}
	id := sess.GetId()
	if _, err := pool.Exec(ctx, `UPDATE sessions SET lifecycle = 'single-shot' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WarmIfIdle(ctx, id); err != nil {
		t.Fatalf("first warm: %v", err)
	}
	if _, err := s.transcr.Append(ctx, id, "human", "go", "discussion", ""); err != nil {
		t.Fatal(err)
	}
	q, err := s.transcr.Append(ctx, id, "agent", `{"questions":[{"question":"which?"}]}`, "question", "")
	if err != nil {
		t.Fatal(err)
	}
	// The pod's last result: it consumed the "go" at seq 0, nothing after.
	if _, err := s.transcr.Append(ctx, id, "worker", `{"subtype":"success","consumedSeq":0,"exit":true}`, "result", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPodPhase(ctx, id, agentfleetv1.PodPhase_POD_PHASE_RUNNING.String(), ""); err != nil {
		t.Fatal(err)
	}

	// The answer, while the phase still reads live: no warm, appended.
	res, err := s.AnswerQuestion(ctx, connect.NewRequest(&agentfleetv1.AnswerQuestionRequest{SessionId: id, Seq: q, AnswersJson: `{"answers":{"which?":"blue"}}`}))
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if n := prov.createCount(); n != 1 {
		t.Fatalf("the answer warmed a pod (%d creates) while the phase read live", n)
	}
	if runs, err := store.ListRewarmRuns(ctx); err != nil || len(runs) != 0 {
		t.Fatalf("listed for a re-warm with the pod still up: %v %v", runs, err)
	}

	// The pod is gone: the run is in the exit window, from consumedSeq 0.
	if err := store.SetPodPhase(ctx, id, agentfleetv1.PodPhase_POD_PHASE_TERMINATED.String(), ""); err != nil {
		t.Fatal(err)
	}
	runs, err := store.ListRewarmRuns(ctx)
	if err != nil || len(runs) != 1 || runs[0] != id {
		t.Fatalf("rewarm runs = %v %v, want %s", runs, err, id)
	}
	if ended, err := store.ListEndedRuns(ctx); err != nil || len(ended) != 0 {
		t.Fatalf("ended before its re-warm: %v %v", ended, err)
	}
	// The client follows up before any loop tick: that sender's own warm
	// must start the next pod at or below the answer, not at LatestSeq.
	if _, err := s.PostMessage(ctx, connect.NewRequest(&agentfleetv1.PostMessageRequest{SessionId: id, Text: "and another thing"})); err != nil {
		t.Fatalf("follow-up: %v", err)
	}
	if n := prov.createCount(); n != 2 {
		t.Fatalf("creates = %d after the follow-up, want the next pod", n)
	}
	prov.mu.Lock()
	next := prov.creates[len(prov.creates)-1]
	prov.mu.Unlock()
	latest, err := s.transcr.LatestSeq(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	answer := res.Msg.GetSeq()
	if from := next.GetResumeFromSeq(); from != 1 || answer < from || latest <= answer {
		t.Fatalf("next pod streams from %d, answer at %d, LatestSeq %d: want the cursor at 1, at or below the answer", from, answer, latest)
	}
	// The new pod is live: no second re-warm, and the run is not ended.
	if runs, err := store.ListRewarmRuns(ctx); err != nil || len(runs) != 0 {
		t.Fatalf("listed again with the next pod up: %v %v", runs, err)
	}
}
