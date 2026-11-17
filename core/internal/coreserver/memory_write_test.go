//go:build integration

package coreserver

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/journal"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// ADR-0086 §5: core reads the STORED set. A writes only to quarantine, C without A to memory, B alone is refused.
func TestAuthorizeMemoryWrite_FollowsTheStoredSet(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := sessions.NewStore(pool)
	srv := New(nil, store, journal.NewStore(pool), nil, nil, nil, nil)
	for _, tc := range []struct {
		set        string
		quarantine bool
	}{
		{"AB", true}, {"AC", true}, {"ABC", true}, {"BC", false},
	} {
		id, err := store.Create(ctx, sessions.CreateParams{Repo: "agent-fleet", Description: "memory", TrustSet: tc.set})
		if err != nil {
			t.Fatal(err)
		}
		resp, err := srv.AuthorizeMemoryWrite(ctx, &agentfleetv1.AuthorizeMemoryWriteRequest{SessionId: id})
		if err != nil || resp.GetQuarantine() != tc.quarantine {
			t.Errorf("%s: %v %v, want quarantine %v", tc.set, resp, err, tc.quarantine)
		}
	}
	if _, err := srv.AuthorizeMemoryWrite(ctx, &agentfleetv1.AuthorizeMemoryWriteRequest{SessionId: "00000000-0000-0000-0000-000000000000"}); status.Code(err) != codes.NotFound {
		t.Errorf("unknown session: got %v, want NotFound", err)
	}
}
