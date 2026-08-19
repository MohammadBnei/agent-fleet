//go:build integration

package dashboard

import (
	"context"
	"net"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/provisionerclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// A provisioner that creates nothing and remembers what it was asked for. The
// only field under test is resume_from_seq — the cursor the next pod will
// stream from.
type recordingProvisioner struct {
	agentfleetv1.UnimplementedProvisionerServiceServer
	mu            sync.Mutex
	calls         int
	resumeFromSeq int64
}

func (p *recordingProvisioner) CreateWorkerPod(_ context.Context, req *agentfleetv1.CreateWorkerPodRequest) (*agentfleetv1.CreateWorkerPodResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.resumeFromSeq = req.GetResumeFromSeq()
	return &agentfleetv1.CreateWorkerPodResponse{PodName: "worker-test"}, nil
}

func (p *recordingProvisioner) snapshot() (int, int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, p.resumeFromSeq
}

// Answering a question on a session whose pod is gone must WARM FIRST, so the
// answer lands at or above the new pod's resume cursor.
//
// This assertion is the entire fix, and it regresses silently if anyone ever
// swaps the two statements in AnswerQuestion.
//
// docs/adr/0050 decided a question outlives its pod: the answer is replayed to
// the next pod from RESUME_FROM_SEQ. That never worked. resumeFromSeq is
// LatestSeq — MAX(seq)+1 — read when the pod is provisioned, so an answer
// appended BEFORE the warm sits one below the cursor and is skipped forever.
// The worker's receiving branch (entry.type === "answer") had never once run.
//
// Nothing about this is visible from outside: the row is written either way,
// pending_decisions drops to zero either way, and the console looks correct
// either way. Only the arithmetic differs.
func TestAnswerQuestion_WarmsFirstSoTheAnswerIsAboveTheResumeCursor(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)

	prov := &recordingProvisioner{}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	gs := grpc.NewServer()
	agentfleetv1.RegisterProvisionerServiceServer(gs, prov)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	provClient, err := provisionerclient.New(lis.Addr().String())
	if err != nil {
		t.Fatalf("provisioner client: %v", err)
	}
	t.Cleanup(func() { _ = provClient.Close() })

	sessionStore := sessions.NewStore(pool)
	repoStore := repos.NewStore(pool)
	transcr := transcript.NewPostgresStore(pool)

	// Only the four collaborators this path touches; the rest of Server is
	// unreachable from AnswerQuestion.
	s := &Server{sessions: sessionStore, repos: repoStore, transcr: transcr, e2e: provClient, maxLive: 5}

	if err := repoStore.Create(ctx, repos.Repo{Name: "editable-blog", URL: "https://example.invalid/x.git"}); err != nil {
		t.Fatalf("repo: %v", err)
	}
	id, err := sessionStore.Create(ctx, sessions.CreateParams{Repo: "editable-blog", Title: "add media uploads"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// The agent asked, then its pod died — the state the operator was left in.
	qSeq, err := transcr.Append(ctx, id, "agent", `{"questions":[]}`, "question", "q-1")
	if err != nil {
		t.Fatalf("question: %v", err)
	}
	if err := sessionStore.SetPodPhase(ctx, id, "POD_PHASE_SUCCEEDED", ""); err != nil {
		t.Fatalf("phase: %v", err)
	}

	res, err := s.AnswerQuestion(ctx, connect.NewRequest(&agentfleetv1.AnswerQuestionRequest{
		SessionId: id, Seq: qSeq, AnswersJson: `{"answers":{"a":"b"}}`,
	}))
	if err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}

	calls, resumeFromSeq := prov.snapshot()
	if calls != 1 {
		t.Fatalf("answering a cold session provisioned %d pods, want 1 — the answer has nothing to wake", calls)
	}
	// The field shape the deleted unit test used to guard, kept here because
	// this is where AnswerQuestion can still be exercised.
	entries, _, err := transcr.ReadSince(ctx, id, 0, 100)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var answer transcript.Entry
	var found bool
	for i := range entries {
		if entries[i].Type == "answer" {
			answer, found = entries[i], true
		}
	}
	if !found {
		t.Fatal("no answer entry was written")
	}
	if answer.From != "human" {
		t.Errorf("answer authored by %q, want human — an answer from anything else would resolve a human's decision on their behalf", answer.From)
	}
	if answer.ReplyTo == nil || *answer.ReplyTo != qSeq {
		t.Errorf("reply_to_seq = %v, want %d (the question's own seq) — without it an answer can be matched to the wrong outstanding decision, reliability-findings.md #0", answer.ReplyTo, qSeq)
	}

	answerSeq := res.Msg.GetSeq()
	if answerSeq < resumeFromSeq {
		t.Errorf("answer at seq %d is BELOW the new pod's cursor %d — it will never be delivered, which is the bug docs/adr/0050 thought it had already solved",
			answerSeq, resumeFromSeq)
	}
}

// The list's summary fetch must return the NEWEST entries, not the oldest.
//
// transcriptWindow treats a zero limit as a forward read from sinceSeq, so the
// dashboard's list — which passed no limit — got entries 0..999 and nothing
// else. Every decision a long-running session raised was therefore invisible
// to it while core, counting over the whole table, still reported the session
// blocked. That asymmetry is exactly what was reported live: a red blocked card
// with no question inside it, and "new questions don't show either" because a
// new question lands at the high end.
//
// A short transcript passes either way, which is why this seeds past the cap.
func TestTranscriptWindow_ALimitedReadReturnsTheNewestEntries(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	sessionStore := sessions.NewStore(pool)
	transcr := transcript.NewPostgresStore(pool)
	repoStore := repos.NewStore(pool)

	if err := repoStore.Create(ctx, repos.Repo{Name: "editable-blog", URL: "https://example.invalid/x.git"}); err != nil {
		t.Fatalf("repo: %v", err)
	}
	id, err := sessionStore.Create(ctx, sessions.CreateParams{Repo: "editable-blog"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Past maxTranscriptPage, so the old forward-read window cannot reach the end.
	const noise = 1200
	for i := 0; i < noise; i++ {
		if _, err := transcr.Append(ctx, id, "agent", "chatter", "assistant", uuid.NewString()); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	qSeq, err := transcr.Append(ctx, id, "agent", `{"questions":[]}`, "question", uuid.NewString())
	if err != nil {
		t.Fatalf("question: %v", err)
	}

	s := &Server{sessions: sessionStore, repos: repoStore, transcr: transcr, maxLive: 5}

	limited, err := s.GetTranscript(ctx, connect.NewRequest(&agentfleetv1.ReadTranscriptSinceRequest{
		SessionId: id, SinceSeq: 0, Limit: proto.Int32(200),
	}))
	if err != nil {
		t.Fatalf("GetTranscript: %v", err)
	}
	var sawQuestion bool
	for _, e := range limited.Msg.GetEntries() {
		if e.GetSeq() == qSeq {
			sawQuestion = true
		}
	}
	if !sawQuestion {
		t.Errorf("the newest-page read missed the question at seq %d in a %d-entry transcript — this is the window the session list uses to decide whether to render a decision",
			qSeq, noise+1)
	}

	// And the shape that caused it: no limit reads from the beginning.
	unlimited, err := s.GetTranscript(ctx, connect.NewRequest(&agentfleetv1.ReadTranscriptSinceRequest{
		SessionId: id, SinceSeq: 0,
	}))
	if err != nil {
		t.Fatalf("GetTranscript unlimited: %v", err)
	}
	entries := unlimited.Msg.GetEntries()
	if len(entries) > 0 && entries[0].GetSeq() != 0 {
		t.Errorf("a no-limit read started at seq %d, want 0 — the forward-read contract other callers depend on has changed", entries[0].GetSeq())
	}
}
