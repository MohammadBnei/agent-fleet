//go:build integration

package dashboard

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// docs/adr/0070 §6, "A parked session takes only its answer": a single-shot
// run with no live pod and an open decision refuses the console's Warm and
// PostMessage (agent.parked, nothing appended, no pod), and its answer warms
// it. A resident session with the same open question is untouched.
func TestParked_TakesOnlyItsAnswer(t *testing.T) {
	ctx := context.Background()
	s, prov, _, store, pool := newDefinitionsServerPool(t)
	s.decisions = decisions.NewStore(pool)

	park := func(lifecycle string) (string, int64) {
		t.Helper()
		sess, err := createDef(s, "dev-dream-analyst", nil)
		if err != nil {
			t.Fatal(err)
		}
		id := sess.GetId()
		if _, err := pool.Exec(ctx, `UPDATE sessions SET lifecycle = $2 WHERE id = $1`, id, lifecycle); err != nil {
			t.Fatal(err)
		}
		if _, err := s.transcr.Append(ctx, id, "human", "go", "discussion", ""); err != nil {
			t.Fatal(err)
		}
		q, err := s.transcr.Append(ctx, id, "agent", `{"questions":[{"question":"which?"}]}`, "question", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.transcr.Append(ctx, id, "worker", `{"subtype":"success","consumedSeq":0,"exit":true}`, "result", ""); err != nil {
			t.Fatal(err)
		}
		if err := store.SetPodPhase(ctx, id, agentfleetv1.PodPhase_POD_PHASE_TERMINATED.String(), ""); err != nil {
			t.Fatal(err)
		}
		return id, q
	}
	id, q := park("single-shot")
	latest, err := s.transcr.LatestSeq(ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	_, werr := s.WarmSession(ctx, connect.NewRequest(&agentfleetv1.WarmSessionRequest{SessionId: id}))
	_, perr := s.PostMessage(ctx, connect.NewRequest(&agentfleetv1.PostMessageRequest{SessionId: id, Text: "hello?"}))
	_, aerr := s.WarmIfIdle(ctx, id)
	for name, err := range map[string]error{"Warm": werr, "PostMessage": perr, "WarmIfIdle": aerr} {
		if code(err) != connect.CodeFailedPrecondition || errkeys.Of(err) != errkeys.KeyParked {
			t.Errorf("%s on a parked run: %v (key %q), want FailedPrecondition agent.parked", name, err, errkeys.Of(err))
		}
	}
	if n := prov.createCount(); n != 0 {
		t.Fatalf("a refused warm created %d pods", n)
	}
	if now, err := s.transcr.LatestSeq(ctx, id); err != nil || now != latest {
		t.Fatalf("a refused PostMessage appended: LatestSeq %d -> %d %v", latest, now, err)
	}

	// The answer warms first, then appends.
	if _, err := s.AnswerQuestion(ctx, connect.NewRequest(&agentfleetv1.AnswerQuestionRequest{SessionId: id, Seq: q, AnswersJson: `{"answers":{"which?":"blue"}}`})); err != nil {
		t.Fatalf("the answer was refused: %v", err)
	}
	if n := prov.createCount(); n != 1 {
		t.Fatalf("the answer created %d pods, want 1", n)
	}

	// A resident session is not parked, whatever it waits on (the run's pod
	// ends first: the definition's concurrency is 1).
	if err := store.SetPodPhase(ctx, id, agentfleetv1.PodPhase_POD_PHASE_TERMINATED.String(), ""); err != nil {
		t.Fatal(err)
	}
	res, _ := park("resident")
	if _, err := s.PostMessage(ctx, connect.NewRequest(&agentfleetv1.PostMessageRequest{SessionId: res, Text: "hello?"})); err != nil {
		t.Fatalf("PostMessage on a resident session with an open question: %v", err)
	}
}
