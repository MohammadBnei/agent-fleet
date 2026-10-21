package agentsvc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/auth"
	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1/agentfleetv1connect"
)

// A new RPC cannot ship unauthorized: every method in the proto has a row.
func TestAuthzTable_CoversEveryMethod(t *testing.T) {
	methods := agentfleetv1.File_agentfleet_v1_agent_proto.Services().ByName("AgentService").Methods()
	for i := 0; i < methods.Len(); i++ {
		proc := "/agentfleet.v1.AgentService/" + string(methods.Get(i).Name())
		if _, ok := table[proc]; !ok {
			t.Errorf("%s has no authz row", proc)
		}
	}
	if len(table) != methods.Len() {
		t.Errorf("table has %d rows for %d methods", len(table), methods.Len())
	}
}

func TestAllowed(t *testing.T) {
	member := Principal{Member: &auth.Identity{Sub: "s"}}
	client := Principal{Client: "bridge"}
	for _, c := range []struct {
		proc string
		p    Principal
		want bool
	}{
		{agentfleetv1connect.AgentServiceCreateThreadProcedure, member, true},
		{agentfleetv1connect.AgentServiceCreateThreadProcedure, client, false},
		{agentfleetv1connect.AgentServiceOpenProcedure, member, false},
		{agentfleetv1connect.AgentServiceOpenProcedure, client, true},
		{agentfleetv1connect.AgentServiceInvokeProcedure, member, false},
		{"/agentfleet.v1.AgentService/Nope", member, false},
		{agentfleetv1connect.AgentServiceListThreadsProcedure, Principal{}, false},
	} {
		if got := allowed(c.proc, c.p); got != c.want {
			t.Errorf("allowed(%s, %+v) = %v, want %v", c.proc, c.p, got, c.want)
		}
	}
}

type fakeHeads []definitions.Head

func (f fakeHeads) List() ([]definitions.Head, []string) { return f, nil }

// The member listener over a real Connect handler: no identity or no sub is
// Unauthenticated, a client-only method is PermissionDenied, and
// ListDefinitions shows only the definitions whose audience the member is in.
func TestMemberListener(t *testing.T) {
	spec := func(group string) *definitions.Spec {
		return &definitions.Spec{Audience: &definitions.Audience{Group: group}, Mode: &definitions.Mode{Allow: []string{"auto", "default"}}}
	}
	srv := &Server{definitions: fakeHeads{
		{Name: "notes", SHA: "a", Effective: spec("shared-notes")},
		{Name: "ops", SHA: "b", Effective: spec("platform-admins")},
		{Name: "broken", Err: "bad"},
	}}
	var id *auth.Identity
	// Stands in for auth's ConnectInterceptor, which sets the identity.
	setID := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if id != nil {
				ctx = auth.WithIdentity(ctx, *id)
			}
			return next(ctx, req)
		}
	})
	path, h := agentfleetv1connect.NewAgentServiceHandler(srv, connect.WithInterceptors(setID, MemberInterceptor()))
	mux := http.NewServeMux()
	mux.Handle(path, h)
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)
	c := agentfleetv1connect.NewAgentServiceClient(hs.Client(), hs.URL)
	ctx := context.Background()
	list := func() ([]string, error) {
		r, err := c.ListDefinitions(ctx, connect.NewRequest(&agentfleetv1.AgentServiceListDefinitionsRequest{}))
		if err != nil {
			return nil, err
		}
		var names []string
		for _, d := range r.Msg.GetDefinitions() {
			names = append(names, d.GetName())
		}
		return names, nil
	}

	if _, err := list(); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("no identity: %v", err)
	}
	id = &auth.Identity{Email: "old@bnei.dev", Groups: []string{"shared-notes"}}
	if _, err := list(); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("empty sub: %v", err)
	}
	id = &auth.Identity{Sub: "m1", Groups: []string{"shared-notes"}}
	if got, err := list(); err != nil || len(got) != 1 || got[0] != "notes" {
		t.Errorf("member: %v, %v", got, err)
	}
	id = &auth.Identity{Sub: "a1", Groups: []string{auth.RequiredGroup}}
	if got, err := list(); err != nil || len(got) != 2 {
		t.Errorf("admin: %v, %v", got, err)
	}
	if _, err := c.Open(ctx, connect.NewRequest(&agentfleetv1.OpenRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("member Open: %v", err)
	}
	if _, err := c.ListThreads(ctx, connect.NewRequest(&agentfleetv1.ListThreadsRequest{})); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Errorf("not yet implemented: %v", err)
	}
}
