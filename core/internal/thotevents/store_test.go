//go:build integration

package thotevents

import (
	"context"
	"testing"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
)

func TestAppendAndReadSince(t *testing.T) {
	ctx := context.Background()
	s := NewStore(dbtest.NewPool(t))

	first, err := s.Append(ctx, KindFinding, "thot", "found something", "")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	second, err := s.Append(ctx, KindAlert, "alertmanager", "EtcdDown", "")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if second <= first {
		t.Fatalf("expected increasing ids, got %d then %d", first, second)
	}

	events, next, err := s.ReadSince(ctx, first, 10)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Kind != KindFinding || events[1].Kind != KindAlert {
		t.Fatalf("unexpected kinds: %q, %q", events[0].Kind, events[1].Kind)
	}
	if next != second+1 {
		t.Fatalf("expected next cursor %d, got %d", second+1, next)
	}
}

// The dedup guarantee: a retried append with the same key must not create
// a second row. Same contract transcript's idempotency_key has.
func TestAppendIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := NewStore(dbtest.NewPool(t))

	a, err := s.Append(ctx, KindFinding, "thot", "once", "same-key")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	b, err := s.Append(ctx, KindFinding, "thot", "once", "same-key")
	if err != nil {
		t.Fatalf("re-append: %v", err)
	}
	if a != b {
		t.Fatalf("expected same id on retry, got %d then %d", a, b)
	}
}

// The load-bearing correlation invariant (docs/adr/0035): a decision
// resolves exactly the request it names, never "any pending request".
func TestPermissionCorrelation(t *testing.T) {
	ctx := context.Background()
	s := NewStore(dbtest.NewPool(t))

	reqA, err := s.Append(ctx, KindPermissionRequest, "thot", `{"tool":"Bash"}`, "")
	if err != nil {
		t.Fatalf("append reqA: %v", err)
	}
	reqB, err := s.Append(ctx, KindPermissionRequest, "thot", `{"tool":"Write"}`, "")
	if err != nil {
		t.Fatalf("append reqB: %v", err)
	}

	pending, err := s.PendingRequests(ctx, 10)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("expected 2 pending, got %d", len(pending))
	}

	// Answer only B.
	if _, err := s.AppendReply(ctx, KindPermissionResponse, "human", DecisionAllow, "", reqB); err != nil {
		t.Fatalf("reply: %v", err)
	}

	got, err := s.FindResponse(ctx, reqB)
	if err != nil {
		t.Fatalf("find B: %v", err)
	}
	if got == nil || got.Payload != DecisionAllow {
		t.Fatalf("expected allow for reqB, got %+v", got)
	}

	// A must still be unanswered — this is the check that would fail if
	// correlation degraded to "any pending + any reply".
	gotA, err := s.FindResponse(ctx, reqA)
	if err != nil {
		t.Fatalf("find A: %v", err)
	}
	if gotA != nil {
		t.Fatalf("reqA must remain unanswered, got %+v", gotA)
	}

	pending, err = s.PendingRequests(ctx, 10)
	if err != nil {
		t.Fatalf("pending after: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != reqA {
		t.Fatalf("expected only reqA pending, got %+v", pending)
	}
}
