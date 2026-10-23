package agentsvc

import (
	"context"
	"slices"
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

// testClients is what fakeHeads reports as the sync's client registrations.
var testClients []definitions.Client

func (f fakeHeads) Clients() []definitions.Client { return testClients }

func (f fakeHeads) MayOpen(client, definition string) (definitions.Head, bool) {
	h, err := f.Resolve(definition)
	if err != nil {
		return definitions.Head{}, false
	}
	for _, c := range testClients {
		if c.Name == client {
			return h, slices.Contains(c.Spec.Definitions, definition) && slices.Contains(h.Effective.Invokers, client)
		}
	}
	return definitions.Head{}, false
}

func (f fakeHeads) Resolve(name string) (definitions.Head, error) {
	for _, h := range f {
		if h.Name == name && h.SHA != "" {
			return h, nil
		}
	}
	return definitions.Head{}, definitions.ErrUnknown
}

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
	path, h := agentfleetv1connect.NewAgentServiceHandler(srv, connect.WithInterceptors(setID, MemberInterceptor("https://fleet.bnei.dev", func() []string { return []string{"https://opus.bnei.dev"} })))
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
}

// docs/adr/0068: an island's call is a member's, inside the definitions that
// embed its origin; an origin nobody lists is refused; an admin's cookie from
// an island grants nothing extra.
func TestOriginFence(t *testing.T) {
	spec := func(group string, origins ...string) *definitions.Spec {
		s := &definitions.Spec{Audience: &definitions.Audience{Group: group}}
		if len(origins) > 0 {
			s.Embed = &definitions.Embed{Origins: origins}
		}
		return s
	}
	srv := &Server{definitions: fakeHeads{
		{Name: "notes", SHA: "a", Effective: spec("shared-notes", "https://opus.bnei.dev")},
		{Name: "other", SHA: "b", Effective: spec("shared-notes")},
		{Name: "ops", SHA: "c", Effective: spec("platform-admins")},
	}}
	var id auth.Identity
	setID := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			return next(auth.WithIdentity(ctx, id), req)
		}
	})
	path, h := agentfleetv1connect.NewAgentServiceHandler(srv, connect.WithInterceptors(setID,
		MemberInterceptor("https://fleet.bnei.dev", func() []string { return []string{"https://opus.bnei.dev"} })))
	mux := http.NewServeMux()
	mux.Handle(path, h)
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)
	c := agentfleetv1connect.NewAgentServiceClient(hs.Client(), hs.URL)
	list := func(origin string) ([]string, error) {
		req := connect.NewRequest(&agentfleetv1.AgentServiceListDefinitionsRequest{})
		if origin != "" {
			req.Header().Set("Origin", origin)
		}
		r, err := c.ListDefinitions(context.Background(), req)
		if err != nil {
			return nil, err
		}
		var names []string
		for _, d := range r.Msg.GetDefinitions() {
			names = append(names, d.GetName())
		}
		return names, nil
	}
	id = auth.Identity{Sub: "m", Groups: []string{"shared-notes"}}
	if got, _ := list(""); len(got) != 2 {
		t.Errorf("member, console: %v", got)
	}
	if got, err := list("https://opus.bnei.dev"); err != nil || len(got) != 1 || got[0] != "notes" {
		t.Errorf("member, island: %v, %v", got, err)
	}
	for _, o := range []string{"https://evil.example", "null", "https://opus.bnei.dev.evil.example"} {
		if _, err := list(o); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("origin %q: %v", o, err)
		}
	}
	id = auth.Identity{Sub: "a", Groups: []string{auth.RequiredGroup}}
	if got, _ := list("https://fleet.bnei.dev"); len(got) != 3 {
		t.Errorf("admin, console: %v", got)
	}
	// Not in shared-notes, so the island shows nothing, admin or not.
	if got, err := list("https://opus.bnei.dev"); err != nil || len(got) != 0 {
		t.Errorf("admin, island: %v, %v", got, err)
	}
}
