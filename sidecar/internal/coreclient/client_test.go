package coreclient

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// grpc.NewClient only validates retryServiceConfig's JSON at call time —
// go build/vet/lint never catch a bad status-code string in it (shipped
// once: "CANCELED" instead of grpc's expected "CANCELLED", which crash-
// looped every pod calling New at startup). This is the cheapest possible
// guard against that recurring silently.
func TestNew_ValidServiceConfig(t *testing.T) {
	c, err := New("localhost:0", "task-1", "lease-1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = c.Close()
}

type fakeRPC struct {
	agentfleetv1.CoreServiceClient
	got *agentfleetv1.AppendJournalRequest
}

func (f *fakeRPC) AppendJournal(_ context.Context, r *agentfleetv1.AppendJournalRequest, _ ...grpc.CallOption) (*agentfleetv1.AppendJournalResponse, error) {
	f.got = r
	return &agentfleetv1.AppendJournalResponse{}, nil
}

// ADR-0086 §5: a pod whose set holds A flags every journal row it writes.
func TestAppendJournal_Quarantine(t *testing.T) {
	for _, q := range []bool{true, false} {
		f := &fakeRPC{}
		c := &Client{rpc: f, Quarantine: q}
		if err := c.AppendJournal(context.Background(), "r", "a", "e", "{}"); err != nil {
			t.Fatal(err)
		}
		if f.got.GetQuarantined() != q {
			t.Errorf("Quarantine=%v sent %v", q, f.got.GetQuarantined())
		}
	}
}

// docs/adr/0084 §4: inside a standing turn the key is core's, never the agent's. Outside one, on a standing pod,
// an effect is refused, and a forged eff: key never passes.
func TestEffectKey_StandingTurn(t *testing.T) {
	c := &Client{taskID: "s1"}
	if k, err := c.effectKey("mine"); err != nil || k != "mine" {
		t.Fatalf("not standing: %q, %v", k, err)
	}
	if _, err := c.effectKey("eff:s1:a:1:1"); err == nil {
		t.Fatal("a forged eff: key passed")
	}
	c.SetTurn("a", 7)
	k1, err1 := c.effectKey("mine")
	k2, err2 := c.effectKey("mine")
	if err1 != nil || err2 != nil || k1 != "eff:s1:a:7:1" || k2 != "eff:s1:a:7:2" {
		t.Fatalf("in a turn: %q %q, %v %v", k1, k2, err1, err2)
	}
	c.SetTurn("", 0)
	if _, err := c.effectKey("mine"); err == nil {
		t.Fatal("an effect outside a turn on a standing pod passed")
	}
}

type reachRPC struct {
	agentfleetv1.CoreServiceClient
	spawn *agentfleetv1.SpawnWorkerRequest
	wait  *agentfleetv1.WaitForSpawnRequest
	issue *agentfleetv1.OpenIssueRequest
	cards *agentfleetv1.ListResponderCardsRequest
	speak *agentfleetv1.SpeakRequest
}

func (f *reachRPC) SpawnWorker(_ context.Context, r *agentfleetv1.SpawnWorkerRequest, _ ...grpc.CallOption) (*agentfleetv1.SpawnWorkerResponse, error) {
	f.spawn = r
	return &agentfleetv1.SpawnWorkerResponse{ThreadId: "t"}, nil
}

func (f *reachRPC) WaitForSpawn(_ context.Context, r *agentfleetv1.WaitForSpawnRequest, _ ...grpc.CallOption) (*agentfleetv1.WaitForResultResponse, error) {
	f.wait = r
	return &agentfleetv1.WaitForResultResponse{State: "done"}, nil
}

func (f *reachRPC) OpenIssue(_ context.Context, r *agentfleetv1.OpenIssueRequest, _ ...grpc.CallOption) (*agentfleetv1.OpenIssueResponse, error) {
	f.issue = r
	return &agentfleetv1.OpenIssueResponse{Url: "u"}, nil
}

func (f *reachRPC) ListResponderCards(_ context.Context, r *agentfleetv1.ListResponderCardsRequest, _ ...grpc.CallOption) (*agentfleetv1.ListResponderCardsResponse, error) {
	f.cards = r
	return &agentfleetv1.ListResponderCardsResponse{}, nil
}

func (f *reachRPC) Speak(_ context.Context, r *agentfleetv1.SpeakRequest, _ ...grpc.CallOption) (*agentfleetv1.SpeakResponse, error) {
	f.speak = r
	return &agentfleetv1.SpeakResponse{Delivered: true}, nil
}

// docs/adr/0084 §6: the session is this pod's, and inside a standing turn a spawn and an issue carry core's effect
// key, never the agent's.
func TestReach_SessionAndEffectKey(t *testing.T) {
	f := &reachRPC{}
	c := &Client{rpc: f, taskID: "s1"}
	ctx := context.Background()
	if _, err := c.SpawnWorker(ctx, "w", "go", "mine"); err != nil || f.spawn.GetSessionId() != "s1" || f.spawn.GetDefinition() != "w" || f.spawn.GetInput() != "go" || f.spawn.GetIdempotencyKey() != "mine" {
		t.Fatalf("spawn: %v %v", err, f.spawn)
	}
	if _, err := c.WaitForResult(ctx, "t1", 20); err != nil || f.wait.GetSessionId() != "s1" || f.wait.GetThreadId() != "t1" || f.wait.GetWaitSeconds() != 20 {
		t.Fatalf("wait: %v %v", err, f.wait)
	}
	if _, err := c.OpenIssue(ctx, "o/r", "T", "B", ""); err != nil || f.issue.GetSessionId() != "s1" || f.issue.GetRepo() != "o/r" || f.issue.GetTitle() != "T" || f.issue.GetBody() != "B" {
		t.Fatalf("issue: %v %v", err, f.issue)
	}
	if _, err := c.ListResponderCards(ctx, "2026-01-01T00:00:00Z", 7); err != nil || f.cards.GetSessionId() != "s1" || f.cards.GetSince() != "2026-01-01T00:00:00Z" || f.cards.GetLimit() != 7 {
		t.Fatalf("cards: %v %v", err, f.cards)
	}
	if d, err := c.Speak(ctx, "hi", "happy", "mine"); err != nil || !d || f.speak.GetSessionId() != "s1" || f.speak.GetText() != "hi" || f.speak.GetFace() != "happy" {
		t.Fatalf("speak: %v %v", err, f.speak)
	}
	c.SetTurn("a", 7)
	if _, err := c.Speak(ctx, "hi", "", "mine"); err != nil || !strings.HasPrefix(f.speak.GetIdempotencyKey(), "eff:s1:a:7:") {
		t.Errorf("a line in a standing turn: %q %v", f.speak.GetIdempotencyKey(), err)
	}
	if _, err := c.SpawnWorker(ctx, "w", "go", "mine"); err != nil || !strings.HasPrefix(f.spawn.GetIdempotencyKey(), "eff:s1:a:7:") {
		t.Errorf("a spawn in a standing turn: %q %v", f.spawn.GetIdempotencyKey(), err)
	}
	if _, err := c.OpenIssue(ctx, "o/r", "T", "B", "mine"); err != nil || !strings.HasPrefix(f.issue.GetIdempotencyKey(), "eff:s1:a:7:") {
		t.Errorf("an issue in a standing turn: %q %v", f.issue.GetIdempotencyKey(), err)
	}
}
