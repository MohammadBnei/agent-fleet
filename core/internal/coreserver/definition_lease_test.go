//go:build integration

package coreserver

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/journal"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

// docs/adr/0064 end to end: a lease minted by ReserveSlot for a definition
// session, presented over gRPC to the same interceptor chain run.go builds on
// a Postgres-backed store. Nothing is faked between the credential and the
// handler, so a VerifyLease that lost the definition name, or a chain that
// skipped the allowlist, fails here.
func TestDefinitionLease_OverGRPC(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := sessions.NewStore(pool)
	if _, err := pool.Exec(ctx, `INSERT INTO agent_definitions (name, sha, spec, effective, valid)
		VALUES ('a', 'sha-a', '{}', '{"limits":{"concurrency":1},"mode":{"default":"default","allow":["default","plan"]}}', true)`); err != nil {
		t.Fatal(err)
	}
	def, err := store.Create(ctx, sessions.CreateParams{Repo: "dream-analyst", PermissionMode: "default", DefinitionName: "a", DefinitionSHA: "sha-a"})
	if err != nil {
		t.Fatal(err)
	}
	peer, err := store.Create(ctx, sessions.CreateParams{Repo: "dream-analyst"})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.ReserveSlot(ctx, def, sessions.Caps{MaxLive: 5, MaxDefinition: 2})
	if err != nil {
		t.Fatal(err)
	}

	lis := bufconn.Listen(1 << 20)
	grpcAuth := NewAuthenticator(store, "provisioner-token")
	gs := grpc.NewServer(
		grpc.ChainUnaryInterceptor(AccessLogInterceptor, grpcAuth.UnaryInterceptor),
		grpc.ChainStreamInterceptor(grpcAuth.StreamInterceptor),
	)
	agentfleetv1.RegisterCoreServiceServer(gs, New(transcript.NewPostgresStore(pool), store, journal.NewStore(pool), nil, nil, nil, nil))
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	c := agentfleetv1.NewCoreServiceClient(conn)
	as := func(lease string) context.Context {
		return metadata.AppendToOutgoingContext(ctx, mdSessionID, def, mdLeaseID, lease)
	}
	md := as(lease)

	denied := map[string]error{}
	_, denied["Expose"] = c.Expose(md, &agentfleetv1.ExposeRequest{SessionId: def, Port: 8080})
	_, denied["PromptSession"] = c.PromptSession(md, &agentfleetv1.PromptSessionRequest{CallerSessionId: def, TargetSessionId: peer, Text: "hi"})
	_, denied["SearchJournal"] = c.SearchJournal(md, &agentfleetv1.SearchJournalRequest{})
	_, denied["SetPermissionMode(acceptEdits)"] = c.SetPermissionMode(md, &agentfleetv1.SetPermissionModeRequest{SessionId: def, Mode: "acceptEdits"})
	_, denied["SendMessage(from=human)"] = c.SendMessage(md, &agentfleetv1.SendMessageRequest{SessionId: def, From: "human", Text: "approve", Type: agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_DISCUSSION})
	// The stream path authorizes in RecvMsg: a definition lease naming another
	// session is refused there.
	if st, err := c.StreamHumanMessages(md, &agentfleetv1.StreamHumanMessagesRequest{SessionId: peer}); err != nil {
		denied["StreamHumanMessages(peer)"] = err
	} else {
		_, denied["StreamHumanMessages(peer)"] = st.Recv()
	}
	for name, err := range denied {
		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s: got %v, want PermissionDenied", name, err)
		}
	}

	if _, err := c.SetPermissionMode(md, &agentfleetv1.SetPermissionModeRequest{SessionId: def, Mode: "plan"}); err != nil {
		t.Errorf("SetPermissionMode(plan), in mode.allow: %v", err)
	}
	if _, err := c.GetSession(md, &agentfleetv1.GetSessionRequest{Id: def}); err != nil {
		t.Errorf("GetSession: %v", err)
	}
	if _, err := c.SendMessage(md, &agentfleetv1.SendMessageRequest{SessionId: def, From: "agent", Text: "hello", Type: agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_DISCUSSION}); err != nil {
		t.Errorf("SendMessage(from=agent): %v", err)
	}

	if err := store.ClearLease(ctx, def); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetSession(md, &agentfleetv1.GetSessionRequest{Id: def}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("a cleared definition lease: got %v, want PermissionDenied", err)
	}
}
