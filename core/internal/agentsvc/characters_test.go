package agentsvc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/auth"
	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1/agentfleetv1connect"
)

// The character API's calls (docs/adr/0084 amendment A1): each is owner only, so each is in both lists and never a
// client's. Add a new character RPC here.
var characterCalls = []string{
	agentfleetv1connect.AgentServiceListCharactersProcedure,
	agentfleetv1connect.AgentServiceWatchCharactersProcedure,
}

func TestCharacterCalls_InBothListsAndNeverAClients(t *testing.T) {
	for _, proc := range characterCalls {
		if a, ok := table[proc]; !ok || !a.member || a.client {
			t.Errorf("%s: table row %+v, want member only", proc, a)
		}
		if !slices.Contains(surfaceCalls, proc) {
			t.Errorf("%s is not in surfaceCalls", proc)
		}
		if allowed(proc, Principal{Client: "game"}) {
			t.Errorf("a registered client may call %s", proc)
		}
	}
}

// asIdentity stands in for auth's interceptor on both kinds of call: it sets the identity the test names.
type asIdentity struct{ id **auth.Identity }

func (a asIdentity) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		return next(auth.WithIdentity(ctx, **a.id), req)
	}
}
func (asIdentity) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}
func (a asIdentity) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		return next(auth.WithIdentity(ctx, **a.id), conn)
	}
}

// The game's call, end to end through the member interceptor: an admin's cookie from the admin origin is allowed,
// and without admin it is denied, as is every other origin and the empty-origin member.
func TestCharacterCalls_Origin(t *testing.T) {
	const game, island = "https://game.bnei.dev", "https://notes.bnei.dev"
	admin := &auth.Identity{Sub: "a", Groups: []string{auth.RequiredGroup}}
	member := &auth.Identity{Sub: "m", Groups: []string{"shared-notes"}}
	id := admin
	srv := &Server{definitions: fakeHeads{}} // no character: the calls reach no store
	path, h := agentfleetv1connect.NewAgentServiceHandler(srv, connect.WithInterceptors(asIdentity{&id},
		MemberInterceptor("https://fleet.bnei.dev", func() []string { return []string{island} }, []string{game})))
	mux := http.NewServeMux()
	mux.Handle(path, h)
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)
	c := agentfleetv1connect.NewAgentServiceClient(hs.Client(), hs.URL)

	list := func(origin string) error {
		req := connect.NewRequest(&agentfleetv1.ListCharactersRequest{})
		if origin != "" {
			req.Header().Set("Origin", origin)
		}
		_, err := c.ListCharacters(context.Background(), req)
		return err
	}
	watch := func(origin string) error {
		req := connect.NewRequest(&agentfleetv1.WatchCharactersRequest{})
		if origin != "" {
			req.Header().Set("Origin", origin)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		st, err := c.WatchCharacters(ctx, req)
		if err != nil {
			return err
		}
		defer st.Close() //nolint:errcheck
		if st.Receive() {
			return nil // the baseline frame
		}
		return st.Err()
	}
	for name, call := range map[string]func(string) error{"ListCharacters": list, "WatchCharacters": watch} {
		for _, tc := range []struct {
			who    string
			id     *auth.Identity
			origin string
			deny   bool
		}{
			{"admin on the admin origin", admin, game, false},
			{"admin in the console", admin, "", false},
			{"member on the admin origin", member, game, true},
			{"member in the console", member, "", true},
			{"admin on an island", admin, island, true},
			{"admin on a stranger", admin, "https://evil.example", true},
		} {
			id = tc.id
			err := call(tc.origin)
			if got := connect.CodeOf(err) == connect.CodePermissionDenied; got != tc.deny || (!tc.deny && err != nil) {
				t.Errorf("%s, %s: %v (denied %v, want %v)", name, tc.who, err, got, tc.deny)
			}
		}
	}
}

func TestCharacterState(t *testing.T) {
	for _, tc := range []struct {
		name       string
		started    bool
		rt         sessions.CharacterRuntime
		held       bool
		state, why string
	}{
		{"never started", false, sessions.CharacterRuntime{}, false, "stopped", ""},
		{"stopped", true, sessions.CharacterRuntime{Stopped: true}, false, "stopped", ""},
		{"stopping", true, sessions.CharacterRuntime{Stopped: true, PodLive: true}, false, "stopping", "pod drain"},
		{"running", true, sessions.CharacterRuntime{PodLive: true, PodRunning: true, RunningThread: "main"}, false, "running", ""},
		{"running through a drain", true, sessions.CharacterRuntime{PodLive: true, PodRunning: true, RunningThread: "main", Draining: true}, false, "running", "pin drain"},
		{"pin drain", true, sessions.CharacterRuntime{PodLive: true, PodRunning: true, Draining: true}, false, "held", "pin drain"},
		{"token refresh", true, sessions.CharacterRuntime{PodLive: true, PodRunning: true, Draining: true, Aged: true}, false, "held", "token refresh"},
		{"crash back-off", true, sessions.CharacterRuntime{Crashes: 2}, false, "held", "crash back-off"},
		{"crashed but warming", true, sessions.CharacterRuntime{Crashes: 1, PodLive: true}, false, "starting", ""},
		{"budget", true, sessions.CharacterRuntime{PodLive: true, PodRunning: true}, true, "held", "budget"},
		{"no pod yet", true, sessions.CharacterRuntime{}, false, "starting", ""},
		{"provisioning", true, sessions.CharacterRuntime{PodLive: true}, false, "starting", ""},
		{"idle", true, sessions.CharacterRuntime{PodLive: true, PodRunning: true}, false, "idle", ""},
	} {
		if state, why := characterState(tc.started, tc.rt, tc.held); state != tc.state || why != tc.why {
			t.Errorf("%s: %q %q, want %q %q", tc.name, state, why, tc.state, tc.why)
		}
	}
}

// Every field of the contract, from a spec and a runtime.
func TestCharacterProto(t *testing.T) {
	spec := &definitions.Spec{
		SidecarTools: []string{"AskUserQuestion", "speak"},
		Spawns:       &definitions.AllowList{Allow: []string{"w"}},
		Limits:       &definitions.Limits{TurnsPerDay: 5, Spawns: &definitions.SpawnLimits{Concurrent: 2}},
		Persona: &definitions.Persona{DisplayName: "Ada", Role: "Keeper", Traits: []string{"calm"}, Portrait: "ada-1", Home: "village:oak",
			Relations: []definitions.Relation{{To: "bo", Kind: "mentor"}}},
	}
	at := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	row := &sessions.Session{}
	rt := sessions.CharacterRuntime{SHA: "s1", PodLive: true, PodRunning: true, TurnsToday: 5, SpawnsLive: 1,
		RunningThread: "chat", RunningSince: at,
		Threads: []sessions.CharacterThread{{Key: "main", Title: "hello", LastAt: at}, {Key: "chat", Running: true, LastAt: at}}}
	c := characterProto("ada", "s1", spec, row, rt, 3)
	if c.GetName() != "ada" || c.GetDisplayName() != "Ada" || c.GetDefinition().GetName() != "ada" || c.GetDefinition().GetSha() != "s1" {
		t.Errorf("identity: %v", c)
	}
	if p := c.GetPersona(); p.GetRole() != "Keeper" || p.GetHome() != "village:oak" || p.GetPortrait() != "ada-1" || p.GetTraits()[0] != "calm" ||
		p.GetRelations()[0].GetTo() != "bo" || p.GetRelations()[0].GetKind() != "mentor" {
		t.Errorf("persona: %v", p)
	}
	if c.GetState() != "running" || !c.GetCanSpeak() || c.GetOpenCards() != 3 {
		t.Errorf("state %q speak %v cards %d", c.GetState(), c.GetCanSpeak(), c.GetOpenCards())
	}
	if b := c.GetBudget(); b.GetTurnsToday() != 5 || b.GetLimit() != 5 || !b.GetHeld() {
		t.Errorf("budget: %v", b)
	}
	if sp := c.GetSpawns(); sp.GetUsed() != 1 || sp.GetLimit() != 2 {
		t.Errorf("spawns: %v", sp)
	}
	if r := c.GetRunningTurn(); r.GetThread() != "chat" || r.GetStartedAt() != "2026-11-18T09:00:00Z" {
		t.Errorf("running turn: %v", r)
	}
	if th := c.GetThreads(); len(th) != 2 || th[0].GetTitle() != "hello" || !th[1].GetRunning() || th[0].GetRunning() || th[0].GetLastAt() != "2026-11-18T09:00:00Z" {
		t.Errorf("threads: %v", th)
	}
	// No persona, no grant, no spawns: the name stands in, and the limits are zero.
	bare := characterProto("zed", "s2", &definitions.Spec{}, nil, sessions.CharacterRuntime{}, 0)
	if bare.GetDisplayName() != "zed" || bare.GetPersona() != nil || bare.GetCanSpeak() || bare.GetSpawns().GetLimit() != 0 || bare.GetRunningTurn() != nil ||
		bare.GetBudget().GetLimit() != int32(definitions.DefaultTurnsPerDay) || bare.GetState() != "stopped" {
		t.Errorf("bare: %v", bare)
	}
}
