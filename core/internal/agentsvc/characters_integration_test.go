//go:build integration

package agentsvc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/dashboard"
	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1/agentfleetv1connect"
)

// docs/adr/0084 amendment A3 over a real database: ListCharacters shows a started character's threads (titles cut to
// 60), running turn, budget, open cards and state, a never-started one as stopped, and a worker definition not at all;
// WatchCharacters sends the baseline and then only what changed.
func TestListAndWatchCharacters(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := sessions.NewStore(pool)
	transcr := transcript.NewPostgresStore(pool, decisions.OnAppend)
	decs := decisions.NewStore(pool)

	adaSpec := &definitions.Spec{
		Lifecycle:    &definitions.AllowList{Allow: []string{"standing"}},
		SidecarTools: []string{"speak"},
		Limits:       &definitions.Limits{TurnsPerDay: 1},
		Persona:      &definitions.Persona{DisplayName: "Ada", Home: "world", Portrait: "ada-1"},
	}
	raw, _ := json.Marshal(adaSpec)
	if _, err := pool.Exec(ctx, `INSERT INTO agent_definitions (name, sha, spec, effective, valid) VALUES ('ada', 'sha1', '{}', $1, true)`, raw); err != nil {
		t.Fatal(err)
	}
	id, err := store.Create(ctx, sessions.CreateParams{Repo: "r", PermissionMode: "default", DefinitionName: "ada", DefinitionSHA: "sha1",
		Owner: "client:standing:ada", Visibility: "private", Lifecycle: sessions.Standing, Invoker: "standing:ada", ThreadKey: "main", StartedBy: "root"})
	if err != nil {
		t.Fatal(err)
	}
	seq := 0
	add := func(thread, from, typ, text string) {
		t.Helper()
		seq++
		if _, err := pool.Exec(ctx, `INSERT INTO transcript (session_id, seq, "from", text, type, idempotency_key, thread_key)
			VALUES ($1, $2, $3, $4, $5, gen_random_uuid()::text, $6)`, id, seq, from, text, typ, thread); err != nil {
			t.Fatal(err)
		}
	}
	for _, th := range []string{"main", "chat"} {
		if _, err := pool.Exec(ctx, `INSERT INTO session_threads (session_id, thread_key) VALUES ($1, $2) ON CONFLICT DO NOTHING`, id, th); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO session_threads (session_id, thread_key, closed_at) VALUES ($1, 'old', now())`, id); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("x", 70)
	add("main", "human", "discussion", "\n  "+long+"\nsecond line")
	add("chat", "human", "discussion", "hello there")
	if _, err := pool.Exec(ctx, `UPDATE sessions SET pod_phase = 'POD_PHASE_RUNNING' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}

	heads := fakeHeads{
		{Name: "ada", SHA: "sha1", Effective: adaSpec},
		{Name: "bo", SHA: "sha2", Effective: &definitions.Spec{Lifecycle: &definitions.AllowList{Allow: []string{"standing"}}}},
		{Name: "worker", SHA: "sha3", Effective: &definitions.Spec{Lifecycle: &definitions.AllowList{Allow: []string{"single-shot"}}}},
	}
	dash := &noWarm{store: store, Server: dashboard.NewServer(store, nil, transcr, nil, nil, nil, nil, nil, sessions.Caps{MaxLive: 5}, nil, nil, nil, "test", "")}
	s := &Server{definitions: heads, dash: dash, sessions: store, transcr: transcr, hub: dashboard.NewHub(), decisions: decs}
	path, h := agentfleetv1connect.NewAgentServiceHandler(s, connect.WithInterceptors(asSub{}))
	mux := http.NewServeMux()
	mux.Handle(path, h)
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)
	c := agentfleetv1connect.NewAgentServiceClient(hs.Client(), hs.URL)
	list := func() map[string]*agentfleetv1.Character {
		t.Helper()
		r, err := c.ListCharacters(ctx, as("root", &agentfleetv1.ListCharactersRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]*agentfleetv1.Character{}
		for _, ch := range r.Msg.GetCharacters() {
			out[ch.GetName()] = ch
		}
		return out
	}

	got := list()
	if len(got) != 2 || got["worker"] != nil {
		t.Fatalf("characters %v, want ada and bo only", got)
	}
	ada := got["ada"]
	if ada.GetState() != "idle" || ada.GetWhy() != "" || ada.GetDisplayName() != "Ada" || ada.GetPersona().GetHome() != "world" || !ada.GetCanSpeak() || ada.GetDefinition().GetSha() != "sha1" {
		t.Errorf("ada: %v", ada)
	}
	if th := ada.GetThreads(); len(th) != 3 || th[0].GetKey() != "main" || th[0].GetTitle() != long[:60] || th[1].GetKey() != "chat" || th[1].GetTitle() != "hello there" || th[0].GetLastAt() == "" ||
		th[0].GetParked() || th[1].GetParked() || th[2].GetKey() != "old" || !th[2].GetParked() {
		t.Errorf("threads: %v, want main, chat, then old parked (open first, main first)", th)
	}
	if b := ada.GetBudget(); b.GetTurnsToday() != 0 || b.GetLimit() != 1 || b.GetHeld() {
		t.Errorf("budget: %v", b)
	}
	if bo := got["bo"]; bo.GetState() != "stopped" || len(bo.GetThreads()) != 0 || bo.GetDefinition().GetSha() != "sha2" {
		t.Errorf("bo: %v", bo)
	}

	// The watch: a baseline of both, then only what changes.
	characterPoll = 50 * time.Millisecond
	t.Cleanup(func() { characterPoll = 2 * time.Second })
	wctx, stop := context.WithTimeout(ctx, 20*time.Second)
	defer stop()
	st, err := c.WatchCharacters(wctx, as("root", &agentfleetv1.WatchCharactersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() //nolint:errcheck
	frame := func() []*agentfleetv1.Character {
		t.Helper()
		if !st.Receive() {
			t.Fatalf("stream ended: %v", st.Err())
		}
		return st.Msg().GetCharacters()
	}
	if base := frame(); len(base) != 2 {
		t.Fatalf("baseline has %d characters, want 2", len(base))
	}

	// A turn runs in chat: agent output above its head entry and above the last result.
	add("chat", "agent", "assistant", "thinking")
	if ch := frame(); len(ch) != 1 || ch[0].GetName() != "ada" || ch[0].GetState() != "running" || ch[0].GetRunningTurn().GetThread() != "chat" ||
		ch[0].GetRunningTurn().GetStartedAt() == "" || !ch[0].GetThreads()[1].GetRunning() {
		t.Fatalf("running: %v", ch)
	}
	// The turn ends; the day's one turn is spent, and core's budget card is open on it.
	add("chat", "worker", "result", `{"subtype":"success"}`)
	if err := s.ReportBudgets(ctx); err != nil {
		t.Fatal(err)
	}
	var ch []*agentfleetv1.Character
	for len(ch) == 0 || ch[0].GetOpenCards() == 0 || ch[0].GetState() != "held" {
		ch = frame()
	}
	if ch[0].GetWhy() != "budget" || !ch[0].GetBudget().GetHeld() || ch[0].GetBudget().GetTurnsToday() != 1 || ch[0].GetOpenCards() != 1 || ch[0].GetRunningTurn() != nil {
		t.Errorf("held: %v", ch[0])
	}
	// Stop with the pod still up: stopping; once it is gone, stopped.
	if ok, err := store.MarkStandingStopped(ctx, id); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ch := frame(); ch[0].GetState() != "stopping" || ch[0].GetWhy() != "pod drain" {
		t.Errorf("stopping: %v", ch)
	}
	if _, err := pool.Exec(ctx, `UPDATE sessions SET pod_phase = NULL WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if ch := frame(); ch[0].GetState() != "stopped" || len(ch) != 1 {
		t.Errorf("stopped: %v", ch)
	}
}

// A character's open cards are those on its own session and on the runs it spawned, cards only, and not on a swept
// session or another character's.
func TestOpenCardsByCharacter(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := sessions.NewStore(pool)
	for _, n := range []string{"ada", "bo"} {
		if _, err := pool.Exec(ctx, `INSERT INTO agent_definitions (name, sha, spec, effective, valid) VALUES ($1, 'sha1', '{}', '{}', true)`, n); err != nil {
			t.Fatal(err)
		}
	}
	mk := func(character, lifecycle, thread string) string {
		t.Helper()
		p := sessions.CreateParams{Repo: "r", PermissionMode: "default", DefinitionName: character, DefinitionSHA: "sha1", Owner: "client:standing:" + character, Visibility: "private",
			Lifecycle: lifecycle, Invoker: "standing:" + character, ThreadKey: thread}
		if lifecycle == sessions.Standing {
			p.StartedBy = "root"
		}
		id, err := store.Create(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	ada, run, swept, bo := mk("ada", sessions.Standing, "main"), mk("ada", "single-shot", "run1"), mk("ada", "single-shot", "run2"), mk("bo", sessions.Standing, "main")
	if _, err := pool.Exec(ctx, `UPDATE sessions SET swept_at = now() WHERE id = $1`, swept); err != nil {
		t.Fatal(err)
	}
	n := 0
	decide := func(session, kind, state string) {
		t.Helper()
		n++
		if _, err := pool.Exec(ctx, `INSERT INTO decisions (kind, session_id, request_seq, state, subject) VALUES ($1, $2, $3, $4, $5)`,
			kind, session, n, state, "character/"+session+kind+state); err != nil {
			t.Fatal(err)
		}
	}
	decide(ada, "card", "open")
	decide(ada, "card", "answered")
	decide(ada, "permission", "open")
	decide(run, "card", "open")
	decide(swept, "card", "open")
	decide(bo, "card", "open")
	got, err := decisions.NewStore(pool).OpenCardsByCharacter(ctx, sessions.StandingPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["ada"] != 2 || got["bo"] != 1 {
		t.Errorf("open cards by character = %v, want ada 2 and bo 1", got)
	}
}

// A started character whose head is invalid or gone is still listed, from its pinned version, and with two turns
// running at once the running turn is a fixed pick (the thread that sorts first).
func TestCharacters_BrokenHeadStillListed_StableRunningTurn(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := sessions.NewStore(pool)
	raw, _ := json.Marshal(&definitions.Spec{Lifecycle: &definitions.AllowList{Allow: []string{"standing"}}, Persona: &definitions.Persona{DisplayName: "Ada", Home: "world"}})
	if _, err := pool.Exec(ctx, `INSERT INTO agent_definitions (name, sha, spec, effective, valid) VALUES ('ada', 'sha1', '{}', $1, true)`, raw); err != nil {
		t.Fatal(err)
	}
	id, err := store.Create(ctx, sessions.CreateParams{Repo: "r", PermissionMode: "default", DefinitionName: "ada", DefinitionSHA: "sha1",
		Owner: "client:standing:ada", Visibility: "private", Lifecycle: sessions.Standing, Invoker: "standing:ada", ThreadKey: "main", StartedBy: "root"})
	if err != nil {
		t.Fatal(err)
	}
	for i, th := range []string{"main", "chat"} {
		if _, err := pool.Exec(ctx, `INSERT INTO session_threads (session_id, thread_key) VALUES ($1, $2) ON CONFLICT DO NOTHING`, id, th); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO transcript (session_id, seq, "from", text, type, idempotency_key, thread_key)
			VALUES ($1, $2, 'human', 'hi', 'discussion', gen_random_uuid()::text, $3), ($1, $4, 'agent', 'busy', 'assistant', gen_random_uuid()::text, $3)`, id, 2*i+1, th, 2*i+2); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{definitions: fakeHeads{}, sessions: store}
	for range 3 {
		cs, err := s.characters(ctx)
		if err != nil || len(cs) != 1 {
			t.Fatalf("characters %v, %v: want the started ada with no head", cs, err)
		}
		if c := cs[0]; c.GetName() != "ada" || c.GetDisplayName() != "Ada" || c.GetState() != "running" || c.GetRunningTurn().GetThread() != "chat" {
			t.Fatalf("ada: %v", c)
		}
	}
}
