//go:build integration

package dashboard

import (
	"context"
	"slices"
	"testing"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// docs/adr/0077: a repo session's model and effort are checked against the
// allowlists, and an empty one stores NULL so Claude Code decides.
func TestCreateSession_ModelAndEffort(t *testing.T) {
	t.Setenv("CLAUDE_MODEL", "")
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := sessions.NewStore(pool)
	s := &Server{sessions: store, repos: repos.NewStore(pool)}
	if _, err := pool.Exec(ctx, `INSERT INTO repos (name, url) VALUES ('notes', 'https://example.invalid/notes.git')`); err != nil {
		t.Fatal(err)
	}
	create := func(model, effort string) (*sessions.Session, error) {
		res, err := s.CreateSession(ctx, connect.NewRequest(&agentfleetv1.CreateSessionRequest{Repo: "notes", Model: &model, Effort: &effort}))
		if err != nil {
			return nil, err
		}
		return store.Get(ctx, res.Msg.GetSession().GetId())
	}

	for _, tc := range []struct{ model, effort string }{{"gpt-5", ""}, {"", "ultra"}, {"", "HIGH"}} {
		if _, err := create(tc.model, tc.effort); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("model %q effort %q: %v, want InvalidArgument", tc.model, tc.effort, err)
		}
	}

	got, err := create("", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != nil || got.Effort != nil {
		t.Errorf("empty choice stored model %v effort %v, want NULL for both", got.Model, got.Effort)
	}

	got, err = create("claude-sonnet-5", "high")
	if err != nil {
		t.Fatal(err)
	}
	if got.Model == nil || *got.Model != "claude-sonnet-5" || got.Effort == nil || *got.Effort != "high" {
		t.Errorf("stored model %v effort %v, want claude-sonnet-5 / high", got.Model, got.Effort)
	}
	if p := SessionToProto(*got); p.GetEffort() != "high" {
		t.Errorf("proto effort %q, want high", p.GetEffort())
	}

	list, err := s.ListRepos(ctx, connect.NewRequest(&agentfleetv1.ListReposRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if m := list.Msg.GetModels(); !slices.Contains(m, "claude-sonnet-5") || slices.Contains(m, "claude-opus-4-5-20251101") {
		t.Errorf("ListRepos models %v, want the aliases without dated snapshots", m)
	}
}
