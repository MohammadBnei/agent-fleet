//go:build integration

package dashboard

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
)

// repos.Store.Get reports "no such repo" as (nil, nil) rather than
// pgx.ErrNoRows — unlike Update and Delete, which do return the sentinel.
// Handling this the way its two neighbours are handled compiles, passes
// against any repo that exists, and panics on the first typo'd name, taking
// core's whole HTTP connection with it. Found by clicking, not by building.
//
// e2e is deliberately nil: reaching the provisioner at all for a repo that
// does not exist is itself the bug, and a nil dereference says so loudly.
func TestSyncRepo_UnknownRepoIsNotFoundNotAPanic(t *testing.T) {
	pool := dbtest.NewPool(t)
	s := &Server{repos: repos.NewStore(pool)}

	_, err := s.SyncRepo(context.Background(), connect.NewRequest(&agentfleetv1.SyncRepoRequest{Name: "no-such-repo"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("want CodeNotFound, got %v (%v)", connect.CodeOf(err), err)
	}

	_, err = s.SyncRepo(context.Background(), connect.NewRequest(&agentfleetv1.SyncRepoRequest{Name: ""}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("want CodeInvalidArgument, got %v (%v)", connect.CodeOf(err), err)
	}
}
