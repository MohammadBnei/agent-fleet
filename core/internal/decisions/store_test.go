//go:build integration

package decisions_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

// Over real Postgres and the real migration (docs/adr/0030): the projection
// the transcript's append hook writes, the paths that close a decision, the
// backfill, and the store's reads.

type fixture struct {
	ctx      context.Context
	pool     *pgxpool.Pool
	tr       *transcript.PostgresStore
	sessions *sessions.Store
	store    *decisions.Store
}

func setup(t *testing.T) fixture {
	t.Helper()
	pool := dbtest.NewPool(t)
	tr := transcript.NewPostgresStore(pool)
	tr.SetAppendHook(decisions.OnAppend)
	return fixture{context.Background(), pool, tr, sessions.NewStore(pool), decisions.NewStore(pool)}
}

func (f fixture) session(t *testing.T) string {
	t.Helper()
	id, err := f.sessions.Create(f.ctx, sessions.CreateParams{Repo: "agent-fleet"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// thread is a definition session, as the deck lists them. Only a client's
// thread has a key (sessions_thread_key_invoker).
func (f fixture) thread(t *testing.T, owner, visibility, threadKey string) string {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO agent_definitions (name, sha, spec, effective, valid)
		VALUES ('d', 'sha', '{}', '{}', true) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := f.pool.QueryRow(f.ctx, `
		INSERT INTO sessions (repo, definition_name, definition_sha, owner, invoker, thread_key, visibility)
		VALUES ('notes', 'd', 'sha', $1, CASE WHEN $1 LIKE 'client:%' THEN substr($1, 8) END, NULLIF($2, ''), $3)
		RETURNING id`, owner, threadKey, visibility).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f fixture) ask(t *testing.T, session, typ, text string) int64 {
	t.Helper()
	seq, err := f.tr.Append(f.ctx, session, "agent", text, typ, "")
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

type row struct {
	Kind, State, Ask, Tool, Canon, Payload, Answer, By string
	AnsweredAt                                       *time.Time
	Version                                          int64
}

func (f fixture) row(t *testing.T, session string, seq int64) *row {
	t.Helper()
	var r row
	err := f.pool.QueryRow(f.ctx, `
		SELECT kind, state, ask, COALESCE(tool, ''), COALESCE(input_canon, ''), COALESCE(payload::text, ''),
		       COALESCE(answer::text, ''), COALESCE(answered_by, ''), answered_at, version
		  FROM decisions WHERE session_id = $1 AND request_seq = $2`, session, seq).
		Scan(&r.Kind, &r.State, &r.Ask, &r.Tool, &r.Canon, &r.Payload, &r.Answer, &r.By, &r.AnsweredAt, &r.Version)
	if err != nil {
		return nil
	}
	return &r
}

func (f fixture) pending(t *testing.T, id string) int {
	t.Helper()
	s, err := f.sessions.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return s.PendingDecisions
}

// A request opens a decision of its kind; a reply closes it, answered by a
// person or withdrawn by the agent; anything else writes nothing.
func TestOnAppend(t *testing.T) {
	f := setup(t)
	id := f.session(t)

	q := f.ask(t, id, "question", `{"questions":[{"question":"which?"}]}`)
	p := f.ask(t, id, "permission_request", `{"tool":"Bash","input":{"timeout":5,"command":"ls"}}`)
	pl := f.ask(t, id, "permission_request", `{"tool":"ExitPlanMode","input":{"plan":"do x"}}`)
	junk := f.ask(t, id, "question", `?`)
	nul := f.ask(t, id, "question", `{"questions":[{"question":"a\u0000b"}]}`)
	other := f.ask(t, id, "discussion", `hello`)

	for _, c := range []struct {
		seq                         int64
		kind, ask, tool, canon, pay string
	}{
		{q, "question", "which?", "", "", `{"questions": [{"question": "which?"}]}`},
		{p, "permission", "Bash", "Bash", `{"command":"ls","timeout":5}`, `{"tool": "Bash", "input": {"command": "ls", "timeout": 5}}`},
		{pl, "plan", "do x", "ExitPlanMode", `{"plan":"do x"}`, `{"tool": "ExitPlanMode", "input": {"plan": "do x"}}`},
		{junk, "question", "", "", "", `"?"`},
			{nul, "question", "ab", "", "", `"{\"questions\":[{\"question\":\"a\\u0000b\"}]}"`},
	} {
		r := f.row(t, id, c.seq)
		if r == nil || r.Kind != c.kind || r.State != "open" || r.Ask != c.ask || r.Tool != c.tool || r.Canon != c.canon || r.Payload != c.pay {
			t.Errorf("seq %d: %+v, want %+v", c.seq, r, c)
		}
	}
	if r := f.row(t, id, other); r != nil {
		t.Errorf("a discussion opened a decision: %+v", r)
	}
	if n := f.pending(t, id); n != 5 {
		t.Fatalf("pending = %d, want 5", n)
	}

	before := f.row(t, id, q).Version
	if _, err := f.tr.AppendAuthored(f.ctx, id, transcript.Author{Sub: "alice", Name: "alice@bnei.dev"}, `{"which?":"a"}`, "answer", "", &q); err != nil {
		t.Fatal(err)
	}
	if r := f.row(t, id, q); r.State != "answered" || r.Answer != `{"which?": "a"}` || r.By != "alice@bnei.dev" || r.AnsweredAt == nil || r.Version <= before {
		t.Errorf("answered question: %+v", r)
	}
	if _, err := f.tr.AppendReply(f.ctx, id, "human", `{"behavior":"deny"}`, "permission_response", "", p); err != nil {
		t.Fatal(err)
	}
	if r := f.row(t, id, p); r.State != "answered" || r.By != "human" || r.Answer != `{"behavior": "deny"}` {
		t.Errorf("answered permission: %+v", r)
	}
	// The agent's own reply is core's supersede: withdrawn, with no answer.
	if _, err := f.tr.AppendReply(f.ctx, id, "agent", "superseded", "answer", "", junk); err != nil {
		t.Fatal(err)
	}
	if r := f.row(t, id, junk); r.State != "withdrawn" || r.Answer != "" || r.By != "" || r.AnsweredAt != nil {
		t.Errorf("withdrawn question: %+v", r)
	}
	// A reply of any type closes the decision, as pending_decisions counted;
	// a later reply to a closed one changes nothing.
	if _, err := f.tr.AppendReply(f.ctx, id, "human", "fine", "discussion", "", nul); err != nil {
		t.Fatal(err)
	}
	if r := f.row(t, id, nul); r.State != "answered" || r.Answer != `"fine"` {
		t.Errorf("discussion reply: %+v", r)
	}
	closed := f.row(t, id, q)
	if _, err := f.tr.AppendReply(f.ctx, id, "agent", "late", "discussion", "", q); err != nil {
		t.Fatal(err)
	}
	if r := f.row(t, id, q); r.Version != closed.Version || r.State != closed.State || r.By != closed.By {
		t.Errorf("a reply to a closed decision changed it: %+v -> %+v", closed, r)
	}
	if n := f.pending(t, id); n != 1 {
		t.Errorf("pending = %d, want 1 (the plan)", n)
	}
	// An idempotent retry writes nothing, so it cannot open a second row.
	if _, err := f.tr.Append(f.ctx, id, "agent", "{}", "question", "k1"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.tr.Append(f.ctx, id, "agent", "{}", "question", "k1"); err != nil {
		t.Fatal(err)
	}
	if n := f.pending(t, id); n != 2 {
		t.Errorf("pending after a retried append = %d, want 2", n)
	}
}

// The re-ask the agent supersedes, through core's own path, is in
// coreserver's TestReuseOrAppendQuestion_WithdrawsTheSupersededDecision.

// A warm closes the dead pod's permissions and plans in the transcript and
// withdraws them here; its questions stay open.
func TestReserveSlot_WithdrawsStaleDecisions(t *testing.T) {
	f := setup(t)
	id := f.session(t)
	p := f.ask(t, id, "permission_request", `{"tool":"Bash","input":{}}`)
	pl := f.ask(t, id, "permission_request", `{"tool":"ExitPlanMode","input":{}}`)
	q := f.ask(t, id, "question", `{"questions":[]}`)
	pv, plv := f.row(t, id, p).Version, f.row(t, id, pl).Version
	if _, err := f.sessions.ReserveSlot(f.ctx, id, sessions.Caps{MaxLive: 5}); err != nil {
		t.Fatal(err)
	}
	if r := f.row(t, id, p); r.State != "withdrawn" || r.Version <= pv {
		t.Errorf("permission after warm: %+v", r)
	}
	if r := f.row(t, id, pl); r.State != "withdrawn" || r.Version <= plv {
		t.Errorf("plan after warm: %+v", r)
	}
	if r := f.row(t, id, q); r.State != "open" {
		t.Errorf("question after warm: %+v", r)
	}
}

// A plan is a permission request, so it holds its pod against the idle
// sweep as a permission does; a question does not.
func TestIdleSweep_APlanHoldsThePod(t *testing.T) {
	f := setup(t)
	idle := func(kind, text string) string {
		t.Helper()
		id := f.session(t)
		if _, err := f.sessions.ReserveSlot(f.ctx, id, sessions.Caps{MaxLive: 5}); err != nil {
			t.Fatal(err)
		}
		if err := f.sessions.SetPodPhase(f.ctx, id, "POD_PHASE_RUNNING", ""); err != nil {
			t.Fatal(err)
		}
		if err := f.sessions.TouchActive(f.ctx, id, "agent", "assistant"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(f.ctx, `UPDATE sessions SET last_active_at = now() - interval '2 hours' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		f.ask(t, id, kind, text)
		return id
	}
	plan := idle("permission_request", `{"tool":"ExitPlanMode","input":{"plan":"p"}}`)
	question := idle("question", `{"questions":[]}`)
	ids, err := f.sessions.ListIdleSessionIDs(f.ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	swept := map[string]bool{}
	for _, id := range ids {
		swept[id] = true
	}
	if swept[plan] || !swept[question] {
		t.Errorf("swept %v: the plan's pod must stay, the question's may go", ids)
	}
}

// Answers racing for one request: one wins, the rest are AlreadyAnswered, and
// the decision is answered once, with the winner's answer.
func TestConcurrentAnswers(t *testing.T) {
	f := setup(t)
	id := f.session(t)
	q := f.ask(t, id, "question", `{"questions":[]}`)
	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			text, _ := json.Marshal(map[string]int{"answer": i})
			_, errs[i] = f.tr.AppendAuthored(f.ctx, id, transcript.Author{Name: "m"}, string(text), "answer", "", &q)
		}()
	}
	wg.Wait()
	won := -1
	for i, err := range errs {
		switch {
		case err == nil && won == -1:
			won = i
		case err == nil:
			t.Fatalf("answers %d and %d both landed", won, i)
		case !errors.Is(err, transcript.ErrAlreadyAnswered):
			t.Fatalf("answer %d: %v", i, err)
		}
	}
	var answered int
	var answer string
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*), max(answer->>'answer') FROM decisions WHERE session_id = $1 AND state = 'answered'`, id).Scan(&answered, &answer); err != nil {
		t.Fatal(err)
	}
	if won == -1 || answered != 1 || answer != string(rune('0'+won)) {
		t.Errorf("answered rows %d with answer %q, winner %d", answered, answer, won)
	}
}

func migration(t *testing.T, name string) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	b, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "..", "db", "migrations", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The backfill, run as the migration runs it: down, the transcript as it was
// left, then up.
func TestBackfill(t *testing.T) {
	f := setup(t)
	if _, err := f.pool.Exec(f.ctx, migration(t, "000023_decisions.down.sql")); err != nil {
		t.Fatalf("down: %v", err)
	}
	live, dead, archived, swept := f.session(t), f.session(t), f.session(t), f.session(t)
	if _, err := f.pool.Exec(f.ctx, `UPDATE sessions SET pod_phase = 'POD_PHASE_RUNNING' WHERE id = $1`, live); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE sessions SET archived_at = now() WHERE id = $1`, archived); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE sessions SET swept_at = now() WHERE id = $1`, swept); err != nil {
		t.Fatal(err)
	}
	// Written by hand: there was no hook, and none may run here.
	add := func(session string, seq int64, typ, text string, replyTo *int64) {
		t.Helper()
		if _, err := f.pool.Exec(f.ctx, `INSERT INTO transcript (session_id, seq, "from", text, type, idempotency_key, reply_to_seq, created_at)
			VALUES ($1, $2, 'agent', $3, $4, gen_random_uuid()::text, $5, '2026-01-01T00:00:00Z'::timestamptz + $6 * interval '1 minute')`,
			session, seq, text, typ, replyTo, float64(seq)); err != nil {
			t.Fatal(err)
		}
	}
	question := `{"questions":[{"question":"which?"}]}`
	bash := `{"tool":"Bash","input":{"command":"ls"}}`
	plan := `{"tool":"ExitPlanMode","input":{"plan":"do x"}}`
	one := int64(1)
	for _, s := range []string{live, dead, archived, swept} {
		add(s, 0, "question", question, nil)
		add(s, 1, "permission_request", bash, nil)
		add(s, 2, "permission_response", `{"behavior":"allow"}`, &one)
		add(s, 3, "permission_request", bash, nil)
		add(s, 4, "permission_request", plan, nil)
		add(s, 5, "permission_request", `not json`, nil)
	}
	if _, err := f.pool.Exec(f.ctx, migration(t, "000023_decisions.up.sql")); err != nil {
		t.Fatalf("up: %v", err)
	}

	want := func(session string, seq int64, kind, state, ask, tool string) {
		t.Helper()
		r := f.row(t, session, seq)
		if r == nil || r.Kind != kind || r.State != state || r.Ask != ask || r.Tool != tool {
			t.Errorf("seq %d: %+v, want %s %s %q %q", seq, r, kind, state, ask, tool)
		}
	}
	want(live, 0, "question", "open", "which?", "")
	want(live, 3, "permission", "open", "Bash", "Bash")
	want(live, 4, "plan", "open", "do x", "ExitPlanMode")
	want(live, 5, "permission", "open", "", "")
	want(dead, 0, "question", "open", "which?", "")
	want(dead, 3, "permission", "withdrawn", "Bash", "Bash")
	want(dead, 4, "plan", "withdrawn", "do x", "ExitPlanMode")
	want(dead, 5, "permission", "withdrawn", "", "")
	if r := f.row(t, dead, 5); r != nil && r.Payload != `"not json"` {
		t.Errorf("unparsable request's payload: %s", r.Payload)
	}
	for _, s := range []string{live, dead} {
		if r := f.row(t, s, 1); r != nil {
			t.Errorf("an answered request was backfilled: %+v", r)
		}
	}
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM decisions WHERE session_id = ANY($1::uuid[])`, []string{archived, swept}).Scan(&n); err != nil || n != 0 {
		t.Errorf("archived or swept sessions backfilled %d rows (%v)", n, err)
	}
	var created time.Time
	if err := f.pool.QueryRow(f.ctx, `SELECT created_at FROM decisions WHERE session_id = $1 AND request_seq = 4`, live).Scan(&created); err != nil || !created.Equal(time.Date(2026, 1, 1, 0, 4, 0, 0, time.UTC)) {
		t.Errorf("created_at is the request's: %v %v", created, err)
	}
	// And the counts read it: the live session waits on four, the dead one on
	// its question.
	if a, b := f.pending(t, live), f.pending(t, dead); a != 4 || b != 1 {
		t.Errorf("pending live %d dead %d, want 4 and 1", a, b)
	}
}

// Visibility as ListThreads filters it, the filters, and newest-first pages.
func TestList(t *testing.T) {
	f := setup(t)
	web := f.thread(t, "client:bridge", "shared", "github/acme/web/pr/1")
	api := f.thread(t, "client:other", "private", "github/acme/api/issue/2")
	mine := f.thread(t, "alice", "private", "")
	console := f.session(t)
	gone := f.thread(t, "alice", "shared", "")
	var all []string
	for _, s := range []string{web, api, mine, console, gone} {
		f.ask(t, s, "question", `{"questions":[]}`)
		f.ask(t, s, "permission_request", `{"tool":"ExitPlanMode","input":{}}`)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE sessions SET archived_at = now() WHERE id = $1`, gone); err != nil {
		t.Fatal(err)
	}
	q := int64(0)
	if _, err := f.tr.AppendReply(f.ctx, web, "human", "{}", "answer", "", q); err != nil {
		t.Fatal(err)
	}

	list := func(fl decisions.Filter) []decisions.Record {
		t.Helper()
		if fl.Limit == 0 {
			fl.Limit = 100
		}
		rows, err := f.store.List(f.ctx, fl)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	sessionsOf := func(rows []decisions.Record) map[string]int {
		out := map[string]int{}
		for _, r := range rows {
			out[r.SessionID]++
		}
		return out
	}
	for _, c := range []struct {
		name string
		f    decisions.Filter
		want map[string]int
	}{
		{"admin", decisions.Filter{Admin: true}, map[string]int{web: 2, api: 2, mine: 2}},
		{"alice", decisions.Filter{Owner: "alice", Shared: true}, map[string]int{web: 2, mine: 2}},
		{"bob", decisions.Filter{Owner: "bob", Shared: true}, map[string]int{web: 2}},
		{"client", decisions.Filter{Owner: "client:other"}, map[string]int{api: 2}},
		{"open", decisions.Filter{Admin: true, States: []string{"open"}}, map[string]int{web: 1, api: 2, mine: 2}},
		{"answered", decisions.Filter{Admin: true, States: []string{"answered", "withdrawn"}}, map[string]int{web: 1}},
		{"plans", decisions.Filter{Admin: true, Kinds: []decisions.Kind{decisions.Plan}}, map[string]int{web: 1, api: 1, mine: 1}},
		{"repo", decisions.Filter{Admin: true, SubjectRepo: "acme/api"}, map[string]int{api: 2}},
		{"thread", decisions.Filter{Admin: true, SessionID: mine}, map[string]int{mine: 2}},
	} {
		got := sessionsOf(list(c.f))
		if len(got) != len(c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
			continue
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("%s: %v, want %v", c.name, got, c.want)
			}
		}
	}
	for _, r := range list(decisions.Filter{Admin: true, SessionID: api}) {
		if r.SubjectRepo != "acme/api" || *r.Owner != "client:other" || *r.Visibility != "private" || *r.Definition != "d" || r.Archived {
			t.Errorf("record's session fields: %+v", r)
		}
	}

	// Pages of two walk all six, newest first, each once.
	whole := list(decisions.Filter{Admin: true})
	var paged []decisions.Record
	var before *decisions.Cursor
	for range 10 {
		page := list(decisions.Filter{Admin: true, Limit: 2, Before: before})
		paged = append(paged, page...)
		if len(page) < 2 {
			break
		}
		last := page[len(page)-1]
		before = &decisions.Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	if len(paged) != 6 || len(whole) != 6 {
		t.Fatalf("paged %d, whole %d, want 6", len(paged), len(whole))
	}
	for i := range whole {
		all = append(all, whole[i].ID)
		if paged[i].ID != whole[i].ID {
			t.Errorf("page order differs at %d", i)
		}
		if i > 0 && whole[i].CreatedAt.After(whole[i-1].CreatedAt) {
			t.Errorf("not newest first at %d", i)
		}
	}

	r, err := f.store.Get(f.ctx, all[0])
	if err != nil || r.ID != all[0] {
		t.Errorf("get: %+v %v", r, err)
	}
	if _, err := f.store.Get(f.ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, decisions.ErrNotFound) {
		t.Errorf("get missing: %v", err)
	}
}

type change struct {
	version int64
	ids     []string
}

func watch(t *testing.T, f fixture, since int64) <-chan change {
	t.Helper()
	decisions.PollEvery = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(f.ctx)
	t.Cleanup(cancel)
	ch := make(chan change, 100)
	go func() {
		_ = f.store.Watch(ctx, since, func(v int64, changed []decisions.Record) error {
			c := change{version: v}
			for _, r := range changed {
				c.ids = append(c.ids, r.ID)
			}
			ch <- c
			return nil
		})
	}()
	return ch
}

func next(t *testing.T, ch <-chan change) change {
	t.Helper()
	select {
	case c := <-ch:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no change within 5s")
	}
	return change{}
}

func (f fixture) id(t *testing.T, session string, seq int64) string {
	t.Helper()
	var id string
	if err := f.pool.QueryRow(f.ctx, `SELECT id::text FROM decisions WHERE session_id = $1 AND request_seq = $2`, session, seq).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// Watch sends the current version, then each change, including one whose
// version was taken before a later one committed.
func TestWatch(t *testing.T) {
	f := setup(t)
	s := f.session(t)
	f.ask(t, s, "question", `{"questions":[]}`)
	ch := watch(t, f, 0)
	base := next(t, ch)
	if base.version == 0 || base.ids != nil {
		t.Fatalf("baseline: %+v", base)
	}

	q := f.ask(t, s, "question", `{"questions":[]}`)
	c := next(t, ch)
	if c.version <= base.version || len(c.ids) != 1 || c.ids[0] != f.id(t, s, q) {
		t.Fatalf("after a question: %+v", c)
	}
	if _, err := f.tr.AppendReply(f.ctx, s, "human", "{}", "answer", "", q); err != nil {
		t.Fatal(err)
	}
	a := next(t, ch)
	if a.version <= c.version || len(a.ids) != 1 || a.ids[0] != c.ids[0] {
		t.Fatalf("after its answer: %+v", a)
	}

	// An append whose transaction holds a lower version past a higher one's
	// commit is still sent once it commits.
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx) //nolint:errcheck
	var slow string
	if err := tx.QueryRow(f.ctx, `INSERT INTO decisions (session_id, request_seq, kind) VALUES ($1, 1000, 'question') RETURNING id::text`, s).Scan(&slow); err != nil {
		t.Fatal(err)
	}
	fast := f.ask(t, s, "question", `{"questions":[]}`)
	if c := next(t, ch); len(c.ids) != 1 || c.ids[0] != f.id(t, s, fast) {
		t.Fatalf("the later commit: %+v", c)
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	if c := next(t, ch); len(c.ids) != 1 || c.ids[0] != slow {
		t.Fatalf("the earlier version, committed late: %+v", c)
	}

	// A resume from a version sends what changed since.
	again := next(t, watch(t, f, base.version))
	if len(again.ids) != 3 {
		t.Errorf("resume from %d: %+v", base.version, again)
	}
}
