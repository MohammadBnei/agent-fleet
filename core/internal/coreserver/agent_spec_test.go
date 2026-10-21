//go:build integration

package coreserver

import (
	"context"
	"net"
	"slices"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/grants"
	"github.com/MohammadBnei/agent-fleet/core/internal/journal"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

// docs/adr/0066 §8 / 0067 §4 over the real interceptor chain: GetSession hands
// a definition worker its spec and sha, and core delivers that session no
// transcript entry unless the request echoes the current sha. A repo session
// gets no spec and is never gated.
func TestAgentSpec_GetSessionAndTheGate(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := sessions.NewStore(pool)
	transcr := transcript.NewPostgresStore(pool)
	if _, err := pool.Exec(ctx, `INSERT INTO agent_definitions (name, sha, spec, effective, valid)
		VALUES ('a', 'sha-a', '{}', '{"limits":{"concurrency":1,"maxTurns":60},"mode":{"default":"default","allow":["default","plan"]},
		        "tools":{"allow":["Read","Bash(git add:*)"],"deny":["Edit(.git/**)"]},"sidecarTools":["send_message","wait_for_messages"],
		        "model":{"default":"claude-opus-5","allow":["claude-opus-5"]},"thinking":{"default":"medium"},
		        "systemPrompt":{"append":"Be brief."}}', true)`); err != nil {
		t.Fatal(err)
	}
	def, err := store.Create(ctx, sessions.CreateParams{Repo: "dream-analyst", PermissionMode: "default", DefinitionName: "a", DefinitionSHA: "sha-a"})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := store.Create(ctx, sessions.CreateParams{Repo: "dream-analyst"})
	if err != nil {
		t.Fatal(err)
	}
	caps := sessions.Caps{MaxLive: 5, MaxDefinition: 2}
	defLease, err := store.ReserveSlot(ctx, def, caps)
	if err != nil {
		t.Fatal(err)
	}
	repoLease, err := store.ReserveSlot(ctx, repo, caps)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{def, repo} {
		if _, err := transcr.Append(ctx, id, "human", "hello", "discussion", ""); err != nil {
			t.Fatal(err)
		}
	}

	lis := bufconn.Listen(1 << 20)
	grpcAuth := NewAuthenticator(store, "provisioner-token")
	gs := grpc.NewServer(
		grpc.ChainUnaryInterceptor(grpcAuth.UnaryInterceptor),
		grpc.ChainStreamInterceptor(grpcAuth.StreamInterceptor),
	)
	svc := New(transcr, store, journal.NewStore(pool), nil, nil, nil, nil)
	svc.SetGrants(grants.NewStore(pool))
	agentfleetv1.RegisterCoreServiceServer(gs, svc)
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
	asDef := metadata.AppendToOutgoingContext(ctx, mdSessionID, def, mdLeaseID, defLease)
	asRepo := metadata.AppendToOutgoingContext(ctx, mdSessionID, repo, mdLeaseID, repoLease)

	got, err := c.GetSession(asDef, &agentfleetv1.GetSessionRequest{Id: def})
	if err != nil {
		t.Fatal(err)
	}
	spec, sha := got.GetAgentSpec(), got.GetAgentSpecSha()
	if spec == nil || len(sha) != 64 {
		t.Fatalf("definition session: spec %v sha %q", spec, sha)
	}
	if !slices.Equal(spec.GetToolsAllow(), []string{"Read", "Bash(git add:*)"}) || !slices.Equal(spec.GetToolsDeny(), []string{"Edit(.git/**)"}) ||
		!slices.Equal(spec.GetSidecarTools(), []string{"send_message", "wait_for_messages"}) || !slices.Equal(spec.GetModeAllow(), []string{"default", "plan"}) ||
		!slices.Equal(spec.GetModelAllow(), []string{"claude-opus-5"}) || spec.GetThinkingDefault() != "medium" ||
		spec.GetSystemPromptAppend() != "Be brief." || spec.GetMaxTurns() != 60 || spec.GetInteraction() != "" || spec.GetLifecycle() != "" {
		t.Errorf("spec not flattened from effective: %v", spec)
	}
	// A grant rides outside the spec (docs/adr/0076 §3): it changes the
	// grants, never the sha, so delivery to a running worker never stops.
	if _, err := grants.NewStore(pool).Add(ctx, def, "a", grants.ScopeSession, "Bash(npm test)", "test"); err != nil {
		t.Fatal(err)
	}
	again, err := c.GetSession(asDef, &agentfleetv1.GetSessionRequest{Id: def})
	if err != nil || !slices.Equal(again.GetGrants(), []string{"Bash(npm test)"}) || len(got.GetGrants()) != 0 {
		t.Errorf("grants: before %v, after %v (%v)", got.GetGrants(), again.GetGrants(), err)
	}
	if err != nil || again.GetAgentSpecSha() != sha {
		t.Errorf("sha not stable: %q then %q (%v)", sha, again.GetAgentSpecSha(), err)
	}
	if r, err := c.GetSession(asRepo, &agentfleetv1.GetSessionRequest{Id: repo}); err != nil || r.GetAgentSpec() != nil || r.GetAgentSpecSha() != "" {
		t.Errorf("repo session: spec %v sha %q err %v, want none", r.GetAgentSpec(), r.GetAgentSpecSha(), err)
	}

	wait := func(md context.Context, id, sha string) (*agentfleetv1.ReadTranscriptSinceResponse, error) {
		return c.WaitForMessages(md, &agentfleetv1.ReadTranscriptSinceRequest{SessionId: id, TimeoutMs: 1, AgentSpecSha: sha})
	}
	stream := func(md context.Context, id, sha string) (*agentfleetv1.TranscriptEntry, error) {
		ctx, cancel := context.WithCancel(md)
		defer cancel()
		st, err := c.StreamHumanMessages(ctx, &agentfleetv1.StreamHumanMessagesRequest{SessionId: id, AgentSpecSha: sha})
		if err != nil {
			return nil, err
		}
		return st.Recv()
	}
	// AskUserQuestion returns a human's answer, so it is gated like the feed.
	ask := func(md context.Context, id, sha string) (*agentfleetv1.AskUserQuestionResponse, error) {
		return c.AskUserQuestion(md, &agentfleetv1.AskUserQuestionRequest{SessionId: id, QuestionsJson: `[{"question":"ok?"}]`, TimeoutMs: 1, AgentSpecSha: sha})
	}
	for _, bad := range []string{"", "x" + sha[1:]} {
		if _, err := ask(asDef, def, bad); status.Code(err) != codes.FailedPrecondition {
			t.Errorf("AskUserQuestion(sha=%q): got %v, want FailedPrecondition", bad, err)
		}
		if _, err := wait(asDef, def, bad); status.Code(err) != codes.FailedPrecondition {
			t.Errorf("WaitForMessages(sha=%q): got %v, want FailedPrecondition", bad, err)
		}
		if _, err := stream(asDef, def, bad); status.Code(err) != codes.FailedPrecondition {
			t.Errorf("StreamHumanMessages(sha=%q): got %v, want FailedPrecondition", bad, err)
		}
	}
	if r, err := wait(asDef, def, sha); err != nil || len(r.GetEntries()) != 1 {
		t.Errorf("WaitForMessages with the applied sha: %v, %v", r, err)
	}
	if e, err := stream(asDef, def, sha); err != nil || e.GetText() != "hello" {
		t.Errorf("StreamHumanMessages with the applied sha: %v, %v", e, err)
	}
	if r, err := ask(asDef, def, sha); err != nil || r.GetQuestionSeq() == 0 {
		t.Errorf("AskUserQuestion with the applied sha: %v, %v", r, err)
	}
	// A repo session ignores the field either way.
	for _, v := range []string{"", sha} {
		if r, err := wait(asRepo, repo, v); err != nil || len(r.GetEntries()) != 1 {
			t.Errorf("repo WaitForMessages(sha=%q): %v, %v", v, r, err)
		}
		if e, err := stream(asRepo, repo, v); err != nil || e.GetText() != "hello" {
			t.Errorf("repo StreamHumanMessages(sha=%q): %v, %v", v, e, err)
		}
	}
	// After the reads above: an asked question is a transcript entry.
	for _, v := range []string{"", sha} {
		if _, err := ask(asRepo, repo, v); err != nil {
			t.Errorf("repo AskUserQuestion(sha=%q): %v", v, err)
		}
	}

	// A revoked pin delivers nothing, sha or not.
	if _, err := pool.Exec(ctx, `UPDATE agent_definitions SET valid = false, error = 'revoked' WHERE name = 'a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := wait(asDef, def, sha); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("revoked pin: got %v, want FailedPrecondition", err)
	}
	if _, err := ask(asDef, def, sha); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("revoked pin AskUserQuestion: got %v, want FailedPrecondition", err)
	}
}
