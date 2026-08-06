//go:build integration

package dashboard

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
	"github.com/MohammadBnei/agent-fleet/core/internal/tasks"
)

// TestServer_CreateTask_UnknownRepo/EmptyDescription cover the two
// validation branches — CreateTask now validates the repo via s.repos.Get
// (docs/adr/0028), a concrete Postgres-backed store, hence the shared
// newTestPool/testcontainers setup instead of a plain unit test with a nil
// store (see stop_integration_test.go's own comment for the same reasoning
// applied to Stop/tasks.Store).
func TestServer_CreateTask_UnknownRepo(t *testing.T) {
	pool := newTestPool(t)
	repoStore := repos.NewStore(pool)
	if err := repoStore.Create(context.Background(), repos.Repo{Name: "dream-analyst", URL: "https://example.com/dream-analyst.git"}); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	s := NewServer(tasks.NewStore(pool), nil, nil, repoStore, nil, nil)

	req := connect.NewRequest(&agentfleetv1.CreateTaskRequest{Repo: "not-a-real-repo", Description: "do something"})
	if _, err := s.CreateTask(context.Background(), req); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("CreateTask error = %v, want CodeInvalidArgument", err)
	}
}

func TestServer_CreateTask_EmptyDescription(t *testing.T) {
	pool := newTestPool(t)
	repoStore := repos.NewStore(pool)
	if err := repoStore.Create(context.Background(), repos.Repo{Name: "dream-analyst", URL: "https://example.com/dream-analyst.git"}); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	s := NewServer(tasks.NewStore(pool), nil, nil, repoStore, nil, nil)

	req := connect.NewRequest(&agentfleetv1.CreateTaskRequest{Repo: "dream-analyst", Description: ""})
	if _, err := s.CreateTask(context.Background(), req); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("CreateTask error = %v, want CodeInvalidArgument", err)
	}
}
