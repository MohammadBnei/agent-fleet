package coreclient

import (
	"context"
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
