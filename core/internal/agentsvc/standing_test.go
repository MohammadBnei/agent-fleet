package agentsvc

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/auth"
	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// docs/adr/0084 §2: only an admin's Start opens a standing definition. Open, Invoke and CreateThread each refuse it
// with agent.standing_only, before anything is claimed or created (the Server has no store: reaching one panics).
func TestStandingDefinitionIsRefusedByEveryOpener(t *testing.T) {
	spec := &definitions.Spec{
		Invokers:  []string{"bot"},
		Lifecycle: &definitions.AllowList{Allow: []string{"standing"}},
	}
	testClients = []definitions.Client{{Name: "bot", Spec: definitions.ClientSpec{Definitions: []string{"ops"}}}}
	t.Cleanup(func() { testClients = nil })
	s := &Server{definitions: fakeHeads{{Name: "ops", SHA: "sha1", Effective: spec}}}
	client := withPrincipal(context.Background(), Principal{Client: "bot"})
	admin := withPrincipal(context.Background(), Principal{Member: &auth.Identity{Sub: "root", Groups: []string{auth.RequiredGroup}}})

	check := func(what string, err error) {
		t.Helper()
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || errkeys.Of(err) != errkeys.KeyStandingOnly {
			t.Errorf("%s: %v (key %q), want FailedPrecondition agent.standing_only", what, err, errkeys.Of(err))
		}
	}
	_, err := s.Open(client, connect.NewRequest(&agentfleetv1.OpenRequest{Definition: "ops", ThreadKey: "k", IdempotencyKey: "i",
		Message: "hi", Visibility: "private", Lifecycle: "resident", Wake: true}))
	check("Open", err)
	_, err = s.Invoke(client, connect.NewRequest(&agentfleetv1.InvokeRequest{Definition: "ops", Input: "x", IdempotencyKey: "i"}))
	check("Invoke", err)
	_, err = s.CreateThread(admin, connect.NewRequest(&agentfleetv1.CreateThreadRequest{Definition: "ops", Visibility: "private"}))
	check("CreateThread", err)
}
