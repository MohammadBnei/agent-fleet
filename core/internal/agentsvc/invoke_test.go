//go:build integration

package agentsvc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

// docs/adr/0070 slice 5 over the :9091 listener and real Postgres: Invoke
// claims once per key, answers a full fleet with CAPACITY and the thread,
// and WaitForResult reads §3's final-turn rule as a long poll; members and
// other clients are refused.
func TestInvoke(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := sessions.NewStore(pool)
	transcr := transcript.NewPostgresStore(pool, decisions.OnAppend)
	if _, err := pool.Exec(ctx, `INSERT INTO repos (name, url) VALUES ('notes', 'https://example.invalid/notes.git')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agent_definitions (name, sha, spec, effective, valid) VALUES ('d', 'sha1', '{}', '{}', true), ('r', 'sha1', '{}', '{}', true)`); err != nil {
		t.Fatal(err)
	}
	spec := func(lifecycles ...string) *definitions.Spec {
		return &definitions.Spec{
			Invokers:  []string{"bot", "other"},
			Workspace: &definitions.Workspace{Kind: "repo", Repo: "notes"},
			Model:     &definitions.Model{Default: "claude-opus-5", Allow: []string{"claude-opus-5"}},
			Mode:      &definitions.Mode{Default: "auto", Allow: []string{"auto"}},
			Lifecycle: &definitions.AllowList{Allow: lifecycles},
		}
	}
	testClients = []definitions.Client{
		{Name: "bot", Spec: definitions.ClientSpec{TokenRef: "BOT", Definitions: []string{"d", "r"}}},
		{Name: "other", Spec: definitions.ClientSpec{TokenRef: "OTHER", Definitions: []string{"d", "r"}}},
	}
	t.Cleanup(func() { testClients = nil })
	tokens := map[string]string{"BOT": strings.Repeat("b", 32), "OTHER": strings.Repeat("o", 32)}
	dash := &noWarm{store: store, Server: dashboard.NewServer(store, nil, transcr, nil, repos.NewStore(pool), nil, nil, nil, sessions.Caps{MaxLive: 5}, nil, nil, nil, "test", "")}
	hub := dashboard.NewHub()
	pollCtx, stop := context.WithCancel(ctx)
	t.Cleanup(stop)
	go hub.PollLoop(pollCtx, transcr, 20*time.Millisecond)
	s := &Server{definitions: fakeHeads{{Name: "d", SHA: "sha1", Effective: spec("resident", "single-shot")}, {Name: "r", SHA: "sha1", Effective: spec("resident")}},
		dash: dash, sessions: store, transcr: transcr, hub: hub, decisions: decisions.NewStore(pool)}

	serve := func(ics ...connect.Interceptor) agentfleetv1connect.AgentServiceClient {
		path, h := agentfleetv1connect.NewAgentServiceHandler(s, connect.WithInterceptors(ics...))
		mux := http.NewServeMux()
		mux.Handle(path, h)
		hs := httptest.NewServer(mux)
		t.Cleanup(hs.Close)
		return agentfleetv1connect.NewAgentServiceClient(hs.Client(), hs.URL)
	}
	clients := serve(ClientInterceptor(func() []definitions.Client { return testClients }, func(k string) string { return tokens[k] }))
	admin := auth.Identity{Sub: "root", Groups: []string{auth.RequiredGroup}}
	members := serve(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			return next(auth.WithIdentity(ctx, admin), req)
		}
	}), MemberInterceptor("https://fleet.bnei.dev", nil, nil))

	bearer := func(tok string) func(connect.AnyRequest) {
		return func(r connect.AnyRequest) { r.Header().Set("Authorization", "Bearer "+tok) }
	}
	bot, other := bearer(tokens["BOT"]), bearer(tokens["OTHER"])
	invoke := func(as func(connect.AnyRequest), def, input, key string) (string, error) {
		r := connect.NewRequest(&agentfleetv1.InvokeRequest{Definition: def, Input: input, IdempotencyKey: key})
		as(r)
		res, err := clients.Invoke(ctx, r)
		if err != nil {
			return "", err
		}
		return res.Msg.GetThreadId(), nil
	}
	wait := func(as func(connect.AnyRequest), id string, secs int32) (*agentfleetv1.WaitForResultResponse, error) {
		r := connect.NewRequest(&agentfleetv1.WaitForResultRequest{ThreadId: id, WaitSeconds: secs})
		as(r)
		res, err := clients.WaitForResult(ctx, r)
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}
	appendEntry := func(id, from, typ, text string) int64 {
		seq, err := transcr.Append(ctx, id, from, text, typ, "")
		if err != nil {
			t.Fatal(err)
		}
		return seq
	}
	code := connect.CodeOf

	if _, err := invoke(bot, "r", "x", "k0"); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("a definition without single-shot: %v", err)
	}
	if _, err := s.Open(withPrincipal(ctx, Principal{Client: "bot"}), connect.NewRequest(&agentfleetv1.OpenRequest{Definition: "d", ThreadKey: "t", Message: "x", Wake: true,
		Lifecycle: "single-shot", Visibility: "private", IdempotencyKey: "o1"})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("Open with single-shot: %v", err)
	}

	// Same key twice runs once; the same key with another input is refused.
	id, err := invoke(bot, "d", "what is 6*7?", "k1")
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	again, err := invoke(bot, "d", "what is 6*7?", "k1")
	if err != nil || again != id || len(dash.warms) != 1 {
		t.Fatalf("retry: %q %v warms=%v", again, err, dash.warms)
	}
	if _, err := invoke(bot, "d", "something else", "k1"); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("same key, other input: %v", err)
	}
	if entries, _, _ := transcr.ReadSince(ctx, id, 0, 10); len(entries) != 1 || entries[0].Text != "what is 6*7?" {
		t.Fatalf("entries: %+v", entries)
	}
	th, err := store.Get(ctx, id)
	if err != nil || deref(th.Lifecycle, "") != "single-shot" || deref(th.Visibility, "") != "private" || deref(th.Owner, "") != "client:bot" {
		t.Fatalf("thread: %+v %v", th, err)
	}

	if r, err := wait(bot, id, 0); err != nil || r.GetState() != "running" {
		t.Fatalf("before any result: %v %v", r, err)
	}
	// A turn that parks on a permission: waiting, whatever its result says.
	req := appendEntry(id, "agent", "permission_request", `{"tool":"Bash","input":{"command":"ls"},"nb":1}`)
	appendEntry(id, "worker", "result", `{"subtype":"success","result_text":"parked","structured_output":{"answer":0},"consumedSeq":0,"exit":true}`)
	r, err := wait(bot, id, 0)
	if err != nil || r.GetState() != "waiting" || len(r.GetPending()) != 1 || r.GetPending()[0].GetRequestSeq() != req || r.GetResultText() != "" {
		t.Fatalf("parked: %v %v", r, err)
	}
	// docs/adr/0070 §4: the thread's Status reads waiting with no pod too.
	if st, err := s.GetStatus(withPrincipal(ctx, Principal{Client: "bot"}), connect.NewRequest(&agentfleetv1.GetStatusRequest{ThreadId: id})); err != nil || st.Msg.GetStatus().GetState() != "waiting" {
		t.Fatalf("GetStatus of a parked run with no pod: %v %v", st, err)
	}
	// The answer is input above the parked result: running until a new one.
	ans := connect.NewRequest(&agentfleetv1.AgentServiceRespondToPermissionRequest{ThreadId: id, Seq: req, Behavior: "allow"})
	bot(ans)
	if _, err := clients.RespondToPermission(ctx, ans); err != nil {
		t.Fatalf("answer: %v", err)
	}
	if r, err := wait(bot, id, 0); err != nil || r.GetState() != "running" {
		t.Fatalf("answered, no new result: %v %v", r, err)
	}
	// Only the pod's exit result is judged (docs/adr/0070 slice 7): a turn's
	// result that is not its pod's last is never final.
	appendEntry(id, "worker", "result", `{"subtype":"success","result_text":"mid-run","consumedSeq":3}`)
	if r, err := wait(bot, id, 0); err != nil || r.GetState() != "running" {
		t.Fatalf("a result without exit: %v %v", r, err)
	}
	// The long poll answers as the final result lands.
	go func() {
		time.Sleep(200 * time.Millisecond)
		appendEntry(id, "worker", "result", `{"subtype":"success","result_text":"42","structured_output":{"answer":42},"consumedSeq":3,"exit":true}`)
	}()
	start := time.Now()
	r, err = wait(bot, id, 30)
	if err != nil || r.GetState() != "done" || r.GetResultText() != "42" || r.GetStructuredOutput() != `{"answer":42}` || r.GetSubtype() != "success" {
		t.Fatalf("done: %v %v", r, err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("the long poll waited %v for a result that landed at once", time.Since(start))
	}

	// Another subtype is final and failed.
	failedID, err := invoke(bot, "d", "loop forever", "k2")
	if err != nil {
		t.Fatal(err)
	}
	appendEntry(failedID, "worker", "result", `{"subtype":"error_max_turns","result_text":"","consumedSeq":0,"exit":true}`)
	if r, err := wait(bot, failedID, 0); err != nil || r.GetState() != "failed" || r.GetSubtype() != "error_max_turns" {
		t.Fatalf("failed: %v %v", r, err)
	}

	// docs/adr/0070 §6, the re-park cap: the eleventh park (limits.maxParks
	// unset, 10) is failed PARK_LIMIT, not waiting, and not the parked turn's
	// success.
	parkedID, err := invoke(bot, "d", "ask a lot", "k-parks")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 11 {
		q := appendEntry(parkedID, "agent", "permission_request", fmt.Sprintf(`{"tool":"Bash","input":{"command":"ls %d"},"nb":1}`, i))
		appendEntry(parkedID, "worker", "result", `{"subtype":"success","result_text":"parked","consumedSeq":100000,"exit":true}`)
		if r, err := wait(bot, parkedID, 0); err != nil || r.GetState() != "waiting" {
			if i < 10 || err != nil || r.GetState() != "failed" || r.GetError().GetCode() != "PARK_LIMIT" {
				t.Fatalf("park %d: %v %v", i+1, r, err)
			}
			break
		}
		if i == 10 {
			t.Fatal("the eleventh park still reads waiting")
		}
		if _, err := transcr.AppendAuthored(ctx, parkedID, transcript.Author{Sub: "client:bot", Name: "client:bot"}, `{"behavior":"deny","message":"no"}`, "permission_response", "", &q); err != nil {
			t.Fatal(err)
		}
	}

	// endRuns' claim withdraws the last park's decision: still PARK_LIMIT.
	if ok, err := store.ClaimEndedRun(ctx, parkedID); err != nil || !ok {
		t.Fatalf("claim the run over its cap: %v %v", ok, err)
	}
	if r, err := wait(bot, parkedID, 0); err != nil || r.GetState() != "failed" || r.GetError().GetCode() != "PARK_LIMIT" {
		t.Fatalf("after the claim: %v %v", r, err)
	}

	// §7: a run torn down, swept and archived still reads its result, for its
	// owner only; one archived without a result reads failed, never running.
	for _, ended := range []string{id, failedID} {
		if err := store.MarkSwept(ctx, ended); err != nil {
			t.Fatal(err)
		}
		if err := store.Archive(ctx, ended); err != nil {
			t.Fatal(err)
		}
	}
	if r, err := wait(bot, id, 0); err != nil || r.GetState() != "done" || r.GetResultText() != "42" {
		t.Fatalf("done after teardown: %v %v", r, err)
	}
	if _, err := wait(other, id, 0); code(err) != connect.CodeNotFound {
		t.Errorf("another client's ended run: %v", err)
	}
	if _, err := s.GetStatus(withPrincipal(ctx, Principal{Client: "bot"}), connect.NewRequest(&agentfleetv1.GetStatusRequest{ThreadId: id})); code(err) != connect.CodeNotFound {
		t.Errorf("an ended run stays hidden from the other thread calls: %v", err)
	}
	crashedID, err := invoke(bot, "d", "crash", "k-crash")
	if err != nil {
		t.Fatal(err)
	}
	// The crash and the archive land during the wait and append nothing:
	// the wait still answers failed well before its deadline.
	endRecheck = 50 * time.Millisecond
	t.Cleanup(func() { endRecheck = 5 * time.Second })
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = store.SetPodPhase(ctx, crashedID, "POD_PHASE_CRASHED", "OOMKilled")
		_ = store.Archive(ctx, crashedID)
	}()
	start = time.Now()
	if r, err := wait(bot, crashedID, 30); err != nil || r.GetState() != "failed" || r.GetError().GetCode() != "AGENT_CRASHED" {
		t.Fatalf("crashed run: %v %v", r, err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("the wait saw the crash after %v", d)
	}
	// A clean exit with no result and no open decision is NO_RESULT, not
	// AGENT_CRASHED (docs/adr/0070 §3, owner answer 6).
	cleanID, err := invoke(bot, "d", "say nothing", "k-clean")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetPodPhase(ctx, cleanID, "POD_PHASE_TERMINATED", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Archive(ctx, cleanID); err != nil {
		t.Fatal(err)
	}
	if r, err := wait(bot, cleanID, 0); err != nil || r.GetState() != "failed" || r.GetError().GetCode() != "NO_RESULT" {
		t.Fatalf("clean exit without a result: %v %v", r, err)
	}
	// docs/adr/0070 §6: a parked run past its answerTimeout reads expired,
	// outranking its parked success result; one whose definition moved
	// reads failed (sessions.Loop's expireRuns ends both).
	for _, c := range []struct{ key, state, code string }{
		{"k-expired", "expired", "ANSWER_TIMEOUT"}, {"k-moved", "failed", "DEFINITION_CHANGED"},
	} {
		runID, err := invoke(bot, "d", c.key, c.key)
		if err != nil {
			t.Fatal(err)
		}
		reqSeq := appendEntry(runID, "agent", "permission_request", `{"tool":"Bash","input":{"command":"ls"},"nb":1}`)
		appendEntry(runID, "worker", "result", `{"subtype":"success","result_text":"parked","consumedSeq":0,"exit":true}`)
		if err := store.SetPodPhase(ctx, runID, "POD_PHASE_TERMINATED", ""); err != nil {
			t.Fatal(err)
		}
		var ended bool
		if c.state == "expired" {
			if _, err := pool.Exec(ctx, `UPDATE decisions SET created_at = now() - interval '25 hours' WHERE session_id = $1`, runID); err != nil {
				t.Fatal(err)
			}
			ended, err = store.ExpireRun(ctx, runID, 24*time.Hour)
		} else {
			ended, err = store.EndMovedRun(ctx, runID)
		}
		if err != nil || !ended {
			t.Fatalf("%s: ended %v %v", c.key, ended, err)
		}
		if r, err := wait(bot, runID, 0); err != nil || r.GetState() != c.state || r.GetError().GetCode() != c.code || r.GetResultText() != "" {
			t.Fatalf("%s: %v %v", c.key, r, err)
		}
		// A late answer through the console (which, unlike a client's thread
		// call, reaches an archived run) is told the decision expired or went
		// stale, not that it was already answered.
		r, err := s.decisions.Request(ctx, runID, reqSeq)
		if err != nil {
			t.Fatal(err)
		}
		wantKey := map[string]any{"expired": errkeys.KeyDecisionExpired, "failed": errkeys.KeyDecisionStale}[c.state]
		_, err = s.decisions.Answer(ctx, nil, nil, r, decisions.Reply{Behavior: "allow"})
		if err = dashboard.AnswerErr(ctx, "late", err); connect.CodeOf(err) != connect.CodeFailedPrecondition || errkeys.Of(err) != wantKey {
			t.Fatalf("%s: late answer: %v key %q, want %q", c.key, err, errkeys.Of(err), wantKey)
		}
	}
	// A run whose pod never wrote an exit result (an older worker at
	// deploy, reaped by the idle sweep) is judged by the pre-slice-7 seq
	// rule once it ended: its latest result, with no input after it.
	legacyID, err := invoke(bot, "d", "old worker", "k-legacy")
	if err != nil {
		t.Fatal(err)
	}
	appendEntry(legacyID, "worker", "result", `{"subtype":"success","result_text":"old"}`)
	if r, err := wait(bot, legacyID, 0); err != nil || r.GetState() != "running" {
		t.Fatalf("legacy result, run not ended: %v %v", r, err)
	}
	if err := store.SetPodPhase(ctx, legacyID, "POD_PHASE_TERMINATED", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Archive(ctx, legacyID); err != nil {
		t.Fatal(err)
	}
	if r, err := wait(bot, legacyID, 0); err != nil || r.GetState() != "done" || r.GetResultText() != "old" {
		t.Fatalf("legacy result, run ended: %v %v", r, err)
	}

	// Only the owning client reads a run, and members call neither method.
	if _, err := wait(other, id, 0); code(err) != connect.CodeNotFound {
		t.Errorf("another client's run: %v", err)
	}
	if _, err := members.WaitForResult(ctx, connect.NewRequest(&agentfleetv1.WaitForResultRequest{ThreadId: id})); code(err) != connect.CodePermissionDenied {
		t.Errorf("a member waits: %v", err)
	}
	if _, err := members.Invoke(ctx, connect.NewRequest(&agentfleetv1.InvokeRequest{Definition: "d", Input: "x", IdempotencyKey: "m1"})); code(err) != connect.CodePermissionDenied {
		t.Errorf("a member invokes: %v", err)
	}
	if _, err := wait(bot, id, 61); code(err) != connect.CodeInvalidArgument {
		t.Errorf("wait_seconds over 60: %v", err)
	}

	// A full fleet: CAPACITY with the thread, the claim pending; the retry
	// with the same key runs on that thread, once.
	dash.warmErr = connect.NewError(connect.CodeResourceExhausted, errors.New("at capacity"))
	_, err = invoke(bot, "d", "later", "k3")
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeResourceExhausted {
		t.Fatalf("over capacity: %v", err)
	}
	capID := ""
	for _, d := range ce.Details() {
		if m, err := d.Value(); err == nil {
			if c, ok := m.(*agentfleetv1.InvokeCapacity); ok {
				capID = c.GetThreadId()
			}
		}
	}
	if capID == "" {
		t.Fatalf("CAPACITY without a thread_id: %v", ce.Details())
	}
	if entries, _, _ := transcr.ReadSince(ctx, capID, 0, 10); len(entries) != 0 {
		t.Fatalf("input appended without a pod: %+v", entries)
	}
	dash.warmErr = nil
	if got, err := invoke(bot, "d", "later", "k3"); err != nil || got != capID {
		t.Fatalf("retry after capacity: %q %v, want %s", got, err, capID)
	}
	if entries, _, _ := transcr.ReadSince(ctx, capID, 0, 10); len(entries) != 1 {
		t.Fatalf("entries after the retry: %+v", entries)
	}

	// Invoke's own cap counts unfinished runs only (capID so far), and runs
	// never count against Open's thread cap.
	if _, err := pool.Exec(ctx, `INSERT INTO sessions (repo, description, definition_name, definition_sha, owner, invoker, visibility, lifecycle)
		SELECT 'notes', '', 'd', 'sha1', 'client:bot', 'bot', 'private', 'single-shot' FROM generate_series(1, $1)`, maxClientRuns-1); err != nil {
		t.Fatal(err)
	}
	if n, err := store.OpenThreads(ctx, "client:bot"); err != nil || n != 0 {
		t.Fatalf("single-shot runs count as open threads: %d %v", n, err)
	}
	// A key cut at maxTitle bytes would split the é: the title stays UTF-8.
	utf8Key := strings.Repeat("a", maxTitle-1) + "é"
	if _, err := invoke(bot, "d", "one more", utf8Key); code(err) != connect.CodeResourceExhausted {
		t.Fatalf("over the run cap: %v", err)
	}
	appendEntry(capID, "worker", "result", `{"subtype":"success","result_text":"ok","consumedSeq":1}`)
	utf8ID, err := invoke(bot, "d", "one more", utf8Key)
	if err != nil {
		t.Fatalf("a finished run frees the cap, multi-byte key: %v", err)
	}
	if th, err := store.Get(ctx, utf8ID); err != nil || deref(th.Title, "") != strings.Repeat("a", maxTitle-1) {
		t.Fatalf("title: %+v %v", th, err)
	}
}

// docs/adr/0070 build order slice 10: with maxParked unset (20), the
// twenty-first parked run is refused with PARKED_LIMIT, not queued; runs with
// a live pod or no open decision, and resident threads, do not count; a
// client's own maxParked replaces the default.
func TestInvoke_ParkedLimit(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := sessions.NewStore(pool)
	transcr := transcript.NewPostgresStore(pool, decisions.OnAppend)
	if _, err := pool.Exec(ctx, `INSERT INTO repos (name, url) VALUES ('notes', 'https://example.invalid/notes.git')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agent_definitions (name, sha, spec, effective, valid) VALUES ('d', 'sha1', '{}', '{}', true)`); err != nil {
		t.Fatal(err)
	}
	two := 2
	testClients = []definitions.Client{
		{Name: "bot", Spec: definitions.ClientSpec{Definitions: []string{"d"}}},
		{Name: "small", Spec: definitions.ClientSpec{Definitions: []string{"d"}, MaxParked: &two}},
	}
	t.Cleanup(func() { testClients = nil })
	dash := &noWarm{store: store, Server: dashboard.NewServer(store, nil, transcr, nil, repos.NewStore(pool), nil, nil, nil, sessions.Caps{MaxLive: 5}, nil, nil, nil, "test", "")}
	s := &Server{definitions: fakeHeads{{Name: "d", SHA: "sha1", Effective: &definitions.Spec{
		Invokers:  []string{"bot", "small"},
		Workspace: &definitions.Workspace{Kind: "repo", Repo: "notes"},
		Model:     &definitions.Model{Default: "claude-opus-5", Allow: []string{"claude-opus-5"}},
		Mode:      &definitions.Mode{Default: "auto", Allow: []string{"auto"}},
		Lifecycle: &definitions.AllowList{Allow: []string{"resident", "single-shot"}},
	}}}, dash: dash, sessions: store, transcr: transcr, hub: dashboard.NewHub(), decisions: decisions.NewStore(pool)}

	invoke := func(client, key string) (string, error) {
		res, err := s.Invoke(withPrincipal(ctx, Principal{Client: client}), connect.NewRequest(&agentfleetv1.InvokeRequest{Definition: "d", Input: "go", IdempotencyKey: key}))
		if err != nil {
			return "", err
		}
		return res.Msg.GetThreadId(), nil
	}
	park := func(id string) {
		if _, err := transcr.Append(ctx, id, "agent", `{"tool":"Bash","input":{"command":"ls"},"nb":1}`, "permission_request", ""); err != nil {
			t.Fatal(err)
		}
	}
	refused := func(err error) bool {
		return connect.CodeOf(err) == connect.CodeResourceExhausted && errkeys.Of(err) == errkeys.KeyParkedLimit
	}
	runs := func(owner string) int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE owner = $1`, owner).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// A resident thread of the same owner waiting on a decision is not a run.
	var resident string
	if err := pool.QueryRow(ctx, `INSERT INTO sessions (repo, description, definition_name, definition_sha, owner, invoker, visibility, lifecycle)
		VALUES ('notes', '', 'd', 'sha1', 'client:bot', 'bot', 'private', 'resident') RETURNING id`).Scan(&resident); err != nil {
		t.Fatal(err)
	}
	park(resident)

	var parked []string
	for i := range definitions.DefaultMaxParked {
		id, err := invoke("bot", fmt.Sprintf("p%d", i))
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		park(id)
		parked = append(parked, id)
	}
	before := runs("client:bot")
	if _, err := invoke("bot", "p21"); !refused(err) {
		t.Fatalf("the twenty-first parked run: %v", err)
	}
	if _, err := invoke("bot", "p21"); !refused(err) || runs("client:bot") != before {
		t.Fatalf("refused, not queued: %v, runs %d want %d", err, runs("client:bot"), before)
	}

	// A run with a live pod is not parked: the same key now runs, once.
	if _, err := pool.Exec(ctx, `UPDATE sessions SET pod_phase = 'POD_PHASE_RUNNING' WHERE id = $1`, parked[0]); err != nil {
		t.Fatal(err)
	}
	id21, err := invoke("bot", "p21")
	if err != nil {
		t.Fatalf("a live-pod run counted as parked: %v", err)
	}
	if entries, _, _ := transcr.ReadSince(ctx, id21, 0, 10); len(entries) != 1 {
		t.Fatalf("p21's input: %+v", entries)
	}
	// p21 has no open decision: not parked either, so one more fits; parked, it fills the cap again.
	id22, err := invoke("bot", "p22")
	if err != nil {
		t.Fatalf("a run with no open decision counted as parked: %v", err)
	}
	park(id22)
	if _, err := invoke("bot", "p23"); !refused(err) {
		t.Fatalf("back at the cap: %v", err)
	}

	// The client's own maxParked replaces the default.
	for i := range two {
		id, err := invoke("small", fmt.Sprintf("s%d", i))
		if err != nil {
			t.Fatalf("small run %d: %v", i, err)
		}
		park(id)
	}
	if _, err := invoke("small", "s2"); !refused(err) {
		t.Fatalf("over maxParked 2: %v", err)
	}
}
