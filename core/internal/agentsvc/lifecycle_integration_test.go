//go:build integration

package agentsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/auth"
	"github.com/MohammadBnei/agent-fleet/core/internal/dashboard"
	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1/agentfleetv1connect"
)

// asGame is the game's call: the admin's identity and the admin-origin principal.
type asGame struct{ member bool }

func (a asGame) ctx(ctx context.Context) context.Context {
	id := auth.Identity{Sub: "root", Email: "root@bnei.dev", Groups: []string{auth.RequiredGroup}}
	if a.member {
		id.Groups = []string{"shared-notes"}
	}
	return withPrincipal(auth.WithIdentity(ctx, id), Principal{Member: &id, Origin: "https://game.bnei.dev", Surface: true})
}
func (a asGame) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		return next(a.ctx(ctx), req)
	}
}
func (asGame) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}
func (a asGame) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error { return next(a.ctx(ctx), conn) }
}

// console stands in for the console's three standing calls, whose persistence dashboard's standing_integration_test
// covers over a real registry: here each answers what the test says, and counts, so the character API's mapping of
// outcomes to states and keys is asserted over a real database.
type fakeConsole struct {
	*noWarm
	start, stop func() error
	closeT      func(key string) error
	calls       int
}

func (f *fakeConsole) StartStanding(context.Context, *connect.Request[agentfleetv1.StartStandingRequest]) (*connect.Response[agentfleetv1.StartStandingResponse], error) {
	f.calls++
	return nil, f.start()
}
func (f *fakeConsole) StopStanding(context.Context, *connect.Request[agentfleetv1.StopStandingRequest]) (*connect.Response[agentfleetv1.StopStandingResponse], error) {
	f.calls++
	return nil, f.stop()
}
func (f *fakeConsole) CloseStandingThread(_ context.Context, r *connect.Request[agentfleetv1.CloseStandingThreadRequest]) (*connect.Response[agentfleetv1.CloseStandingThreadResponse], error) {
	f.calls++
	return nil, f.closeT(r.Msg.GetThreadKey())
}

func consoleErr(code connect.Code, sentinel error) error {
	return errkeys.Connect(code, errkeys.KeyInternal, fmt.Errorf("%q: %w", "ada", sentinel))
}

// docs/adr/0084 amendment A4 and A5 over a real database: Start and Stop answer the character's state at once, and an
// outcome that only says the state already is what was asked (started, stopping, stopped, never started, a parked
// thread) answers the state, not an error; only the owner reaches the console's calls; a close parks a thread, which
// the owner's next message reopens with its session id; a thread read returns the full transcript of one thread.
func TestCharacterLifecycleAndThreads(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := sessions.NewStore(pool)
	transcr := transcript.NewPostgresStore(pool, decisions.OnAppend)
	spec := &definitions.Spec{Lifecycle: &definitions.AllowList{Allow: []string{"standing"}}}
	raw, _ := json.Marshal(spec)
	if _, err := pool.Exec(ctx, `INSERT INTO agent_definitions (name, sha, spec, effective, valid) VALUES ('ada', 'sha1', '{}', $1, true)`, raw); err != nil {
		t.Fatal(err)
	}
	heads := fakeHeads{{Name: "ada", SHA: "sha1", Effective: spec}, {Name: "worker", SHA: "sha3", Effective: &definitions.Spec{Lifecycle: &definitions.AllowList{Allow: []string{"single-shot"}}}}}
	dash := &fakeConsole{noWarm: &noWarm{store: store, Server: dashboard.NewServer(store, nil, transcr, nil, repos.NewStore(pool), nil, nil, nil, sessions.Caps{MaxLive: 5}, nil, nil, nil, "test", "")}}
	s := &Server{definitions: heads, dash: dash, sessions: store, transcr: transcr, hub: dashboard.NewHub(), decisions: decisions.NewStore(pool), speech: newSpeechHub()}
	serve := func(a asGame) agentfleetv1connect.AgentServiceClient {
		path, h := agentfleetv1connect.NewAgentServiceHandler(s, connect.WithInterceptors(a))
		mux := http.NewServeMux()
		mux.Handle(path, h)
		hs := httptest.NewServer(mux)
		t.Cleanup(hs.Close)
		return agentfleetv1connect.NewAgentServiceClient(hs.Client(), hs.URL)
	}
	c, member := serve(asGame{}), serve(asGame{member: true})

	start := func(name string) (*agentfleetv1.Character, error) {
		r, err := c.StartCharacter(ctx, connect.NewRequest(&agentfleetv1.StartCharacterRequest{Name: name}))
		if err != nil {
			return nil, err
		}
		return r.Msg.GetCharacter(), nil
	}
	stop := func(name string) (*agentfleetv1.Character, error) {
		r, err := c.StopCharacter(ctx, connect.NewRequest(&agentfleetv1.StopCharacterRequest{Name: name}))
		if err != nil {
			return nil, err
		}
		return r.Msg.GetCharacter(), nil
	}
	state := func(ch *agentfleetv1.Character, err error, want, why string) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if ch.GetState() != want || ch.GetWhy() != why {
			t.Fatalf("state %q (%q), want %q (%q)", ch.GetState(), ch.GetWhy(), want, why)
		}
	}
	notFound := func(err error, key string) {
		t.Helper()
		if connect.CodeOf(err) != connect.CodeNotFound || string(errkeys.Of(err)) != key {
			t.Fatalf("%v (key %q), want NotFound %s", err, errkeys.Of(err), key)
		}
	}

	// Never started: Stop is the state it already is. Unknown names and a worker definition are not characters, and
	// the console is never reached for them.
	dash.stop = func() error { return consoleErr(connect.CodeNotFound, dashboard.ErrStandingNever) }
	ch, err := stop("ada")
	state(ch, err, "stopped", "")
	for _, name := range []string{"ghost", "worker"} {
		_, err := start(name)
		notFound(err, "agent.character_not_found")
		_, err = stop(name)
		notFound(err, "agent.character_not_found")
	}
	if dash.calls != 1 {
		t.Fatalf("the console was called %d times, want 1 (Stop of ada)", dash.calls)
	}

	// The row, started, with a pod coming up.
	id, err := store.Create(ctx, sessions.CreateParams{Repo: "r", PermissionMode: "default", DefinitionName: "ada", DefinitionSHA: "sha1",
		Owner: "client:standing:ada", Visibility: "private", Lifecycle: sessions.Standing, Invoker: "standing:ada", ThreadKey: "main", StartedBy: "root"})
	if err != nil {
		t.Fatal(err)
	}
	// Started: its state, whether the console says "already started" or does it.
	dash.start = func() error { return consoleErr(connect.CodeFailedPrecondition, dashboard.ErrStandingStarted) }
	ch, err = start("ada")
	state(ch, err, "starting", "")
	// Stopping: the old pod is still going down. Not an error; the game shows it and retries.
	if _, err := pool.Exec(ctx, `UPDATE sessions SET stopped_at = now(), pod_phase = 'POD_PHASE_RUNNING' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	dash.start = func() error { return consoleErr(connect.CodeAborted, dashboard.ErrStandingStopping) }
	ch, err = start("ada")
	state(ch, err, "stopping", "pod drain")
	dash.stop = func() error { return consoleErr(connect.CodeFailedPrecondition, dashboard.ErrStandingStopped) }
	ch, err = stop("ada")
	state(ch, err, "stopping", "pod drain")
	// Stopped, then a start that works.
	if _, err := pool.Exec(ctx, `UPDATE sessions SET pod_phase = 'POD_PHASE_TERMINATED' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	ch, err = stop("ada")
	state(ch, err, "stopped", "")
	dash.start = func() error {
		_, err := pool.Exec(ctx, `UPDATE sessions SET stopped_at = NULL WHERE id = $1`, id)
		return err
	}
	ch, err = start("ada")
	state(ch, err, "starting", "")
	// Any other failure is the console's, unchanged.
	dash.start = func() error {
		return errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, errors.New("head invalid"))
	}
	if _, err := start("ada"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("a refused start: %v", err)
	}

	// Only the owner reaches the console's calls.
	before := dash.calls
	if _, err := member.StartCharacter(ctx, connect.NewRequest(&agentfleetv1.StartCharacterRequest{Name: "ada"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a member's Start: %v", err)
	}
	if _, err := member.StopCharacter(ctx, connect.NewRequest(&agentfleetv1.StopCharacterRequest{Name: "ada"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a member's Stop: %v", err)
	}
	if _, err := member.CloseCharacterThread(ctx, connect.NewRequest(&agentfleetv1.CloseCharacterThreadRequest{Name: "ada", Thread: "chat"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a member's Close: %v", err)
	}
	if dash.calls != before {
		t.Errorf("a refused call reached the console %d times", dash.calls-before)
	}

	// Three threads: main with a turn, chat with a kept SDK session, notes parked with one.
	add := func(thread, from, typ, text string) int {
		t.Helper()
		var seq int
		if err := pool.QueryRow(ctx, `INSERT INTO transcript (session_id, seq, "from", text, type, idempotency_key, thread_key)
			VALUES ($1, (SELECT COALESCE(max(seq) + 1, 0) FROM transcript WHERE session_id = $1), $2, $3, $4, gen_random_uuid()::text, $5) RETURNING seq`,
			id, from, text, typ, thread).Scan(&seq); err != nil {
			t.Fatal(err)
		}
		return seq
	}
	for _, th := range []string{"main", "chat", "notes"} {
		if _, err := pool.Exec(ctx, `INSERT INTO session_threads (session_id, thread_key) VALUES ($1, $2) ON CONFLICT DO NOTHING`, id, th); err != nil {
			t.Fatal(err)
		}
	}
	head := add("main", "human", "discussion", "work on the map")
	add("main", "agent", "tool_call", `{"content":"42 files"}`)
	waiting := add("chat", "human", "discussion", "when you are free")
	notesHead := add("notes", "human", "discussion", "a note")
	if _, err := pool.Exec(ctx, `UPDATE session_threads SET cursor = $2, agent_session_id = 'sdk-chat' WHERE session_id = $1 AND thread_key = 'chat'`, id, waiting); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE session_threads SET cursor = $2, agent_session_id = 'sdk-notes', closed_at = now() WHERE session_id = $1 AND thread_key = 'notes'`, id, notesHead+1); err != nil {
		t.Fatal(err)
	}
	threadRow := func(key string) (cursor int64, sdk string, closed bool) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT cursor, COALESCE(agent_session_id, ''), closed_at IS NOT NULL FROM session_threads WHERE session_id = $1 AND thread_key = $2`, id, key).Scan(&cursor, &sdk, &closed); err != nil {
			t.Fatal(err)
		}
		return
	}

	// Close: a parked thread is a success, main and an unknown key are not threads, an unknown character is none.
	closeThread := func(name, key string) error {
		_, err := c.CloseCharacterThread(ctx, connect.NewRequest(&agentfleetv1.CloseCharacterThreadRequest{Name: name, Thread: key}))
		return err
	}
	dash.closeT = func(string) error { return nil }
	if err := closeThread("ada", "chat"); err != nil {
		t.Fatal(err)
	}
	dash.closeT = func(string) error { return consoleErr(connect.CodeFailedPrecondition, dashboard.ErrThreadParked) }
	if err := closeThread("ada", "notes"); err != nil {
		t.Errorf("closing a parked thread: %v", err)
	}
	dash.closeT = func(string) error { return consoleErr(connect.CodeFailedPrecondition, dashboard.ErrNoThread) }
	notFound(closeThread("ada", "main"), "agent.thread_not_found")
	dash.closeT = func(string) error { return consoleErr(connect.CodeNotFound, dashboard.ErrStandingNever) }
	notFound(closeThread("ada", "chat"), "agent.thread_not_found")
	notFound(closeThread("ghost", "chat"), "agent.character_not_found")

	// The owner's next message to a parked key reopens it, keeping its session id.
	post, err := c.PostToCharacter(ctx, connect.NewRequest(&agentfleetv1.PostToCharacterRequest{Character: "ada", Text: "back to it", ThreadKey: "notes"}))
	if err != nil {
		t.Fatal(err)
	}
	if cur, sdk, closed := threadRow("notes"); closed || sdk != "sdk-notes" || cur != post.Msg.GetSeq() {
		t.Errorf("notes after the message: cursor %d session %q closed %v, want reopened at %d with sdk-notes", cur, sdk, closed, post.Msg.GetSeq())
	}

	// A thread read: that thread's entries only, tool results included, parked threads too.
	read := func(name, key string, since int64, limit int32) (*agentfleetv1.ReadCharacterThreadResponse, error) {
		r, err := c.ReadCharacterThread(ctx, connect.NewRequest(&agentfleetv1.ReadCharacterThreadRequest{Name: name, Thread: key, Since: since, Limit: limit}))
		if err != nil {
			return nil, err
		}
		return r.Msg, nil
	}
	got, err := read("ada", "main", 0, 0)
	if err != nil || len(got.GetEntries()) != 2 || got.GetEntries()[0].GetText() != "work on the map" || got.GetEntries()[1].GetText() != `{"content":"42 files"}` {
		t.Fatalf("main read: %v, %v", got, err)
	}
	if got, err = read("ada", "chat", 0, 0); err != nil || len(got.GetEntries()) != 1 || got.GetEntries()[0].GetText() != "when you are free" {
		t.Errorf("a parked thread's read: %v, %v", got, err)
	}
	p1, err := read("ada", "main", 0, 1) // paging: one entry, and where to go on
	if err != nil || len(p1.GetEntries()) != 1 || p1.GetNextSeq() != int64(head)+1 {
		t.Fatalf("page 1: %v, %v", p1, err)
	}
	if p2, err := read("ada", "main", p1.GetNextSeq(), 1); err != nil || len(p2.GetEntries()) != 1 || p2.GetEntries()[0].GetText() != `{"content":"42 files"}` {
		t.Errorf("page 2: %v, %v", p2, err)
	}
	_, err = read("ada", "nope", 0, 0)
	notFound(err, "agent.thread_not_found")
	_, err = read("ghost", "main", 0, 0)
	notFound(err, "agent.character_not_found")
	if _, err := member.ReadCharacterThread(ctx, connect.NewRequest(&agentfleetv1.ReadCharacterThreadRequest{Name: "ada", Thread: "main"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a member's thread read: %v", err)
	}

	// The watch: a first frame of what is there, then each new entry of the thread and no other.
	threadPoll = 50 * time.Millisecond
	t.Cleanup(func() { threadPoll = time.Second })
	wctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	st, err := c.WatchCharacterThread(wctx, connect.NewRequest(&agentfleetv1.WatchCharacterThreadRequest{Name: "ada", Thread: "chat"}))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() //nolint:errcheck
	if !st.Receive() || len(st.Msg().GetEntries()) != 1 {
		t.Fatalf("first frame: %v (%v)", st.Msg(), st.Err())
	}
	add("main", "agent", "tool_call", "another thread")
	add("chat", "agent", "tool_call", "late result")
	if !st.Receive() || len(st.Msg().GetEntries()) != 1 || st.Msg().GetEntries()[0].GetText() != "late result" {
		t.Fatalf("second frame: %v (%v)", st.Msg(), st.Err())
	}
}
