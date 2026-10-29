//go:build integration

package decisions_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

// effects records what an answer asked of the console.
type effects struct {
	warms, modes []string
}

func (e *effects) WarmIfIdle(_ context.Context, id string) (string, error) {
	e.warms = append(e.warms, id)
	return "", nil
}

func (e *effects) SetMode(_ context.Context, _ string, mode string) error {
	e.modes = append(e.modes, mode)
	return nil
}

func (e *effects) RememberGrant(context.Context, string, int64, string, string, string) (string, func(), error) {
	return "", nil, errors.New("no standing rules here")
}

var human = transcript.Author{Sub: "u1", Name: "owner@bnei.dev"}

func (f fixture) answer(t *testing.T, fx *effects, session string, seq int64, a decisions.Reply) (string, error) {
	t.Helper()
	r, err := f.store.Request(f.ctx, session, seq)
	if err != nil {
		t.Fatal(err)
	}
	a.By = human
	n, err := f.store.Answer(f.ctx, fx, f.tr, r, a)
	if err != nil {
		return "", err
	}
	e, _, err := f.tr.ReadSince(f.ctx, session, n, 1)
	if err != nil || len(e) != 1 {
		t.Fatalf("the answer at %d: %v, %v", n, e, err)
	}
	return e[0].Text, nil
}

func (f fixture) grantable(t *testing.T, session string, seq int64) {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx, `UPDATE decisions SET grantable = true WHERE session_id = $1 AND request_seq = $2`, session, seq); err != nil {
		t.Fatal(err)
	}
}

func (f fixture) consume(t *testing.T, session, tool, input string) string {
	t.Helper()
	id, err := f.store.ConsumeGrant(f.ctx, session, tool, []byte(input))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

const bigInput = `{"command":"ls -la","n":12345678901234567890}`

// One path per kind (docs/adr/0082 §5): a question warms then answers; a
// permission answers without a warm (the stale-close would withdraw it); a
// plan approval switches to the mode it names first, and to none when it
// names none, without a warm either.
func TestAnswer_OnePathPerKind(t *testing.T) {
	f := setup(t)
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO agent_definitions (name, sha, spec, effective, valid)
		VALUES ('p', 'sha', '{}', '{"mode":{"default":"plan","allow":["auto","default","plan"]}}', true)`); err != nil {
		t.Fatal(err)
	}
	thread := func(mode string) string {
		var id string
		if err := f.pool.QueryRow(f.ctx, `INSERT INTO sessions (repo, definition_name, definition_sha, owner, permission_mode)
			VALUES ('notes', 'p', 'sha', 'u1', $1) RETURNING id`, mode).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	inPlan, inAuto, repo := thread("plan"), thread("auto"), f.session(t)

	fx := &effects{}
	q := f.ask(t, repo, "question", `{"questions":[{"question":"which?"}]}`)
	if text, err := f.answer(t, fx, repo, q, decisions.Reply{Answers: `{"answers":{"which?":"a"}}`}); err != nil || text != `{"answers":{"which?":"a"}}` {
		t.Fatalf("question: %q, %v", text, err)
	}
	if !slices.Equal(fx.warms, []string{repo}) {
		t.Errorf("question warms = %v, want the session once", fx.warms)
	}
	if r := f.row(t, repo, q); r.State != "answered" || r.By != "owner@bnei.dev" {
		t.Errorf("question row: %+v", r)
	}

	fx = &effects{}
	p := f.ask(t, repo, "permission_request", `{"tool":"Bash","input":{"command":"ls"}}`)
	if text, err := f.answer(t, fx, repo, p, decisions.Reply{Behavior: "deny", Message: "no"}); err != nil || text != `{"behavior":"deny","message":"no"}` {
		t.Fatalf("permission: %q, %v", text, err)
	}
	if fx.warms != nil {
		t.Errorf("a permission answer warmed %v", fx.warms)
	}
	if _, err := f.answer(t, fx, repo, p, decisions.Reply{Behavior: "allow"}); !errors.Is(err, transcript.ErrAlreadyAnswered) {
		t.Errorf("second answer: %v", err)
	}
	maybe := f.ask(t, repo, "permission_request", `{"tool":"Bash","input":{"command":"ls"}}`)
	if _, err := f.answer(t, fx, repo, maybe, decisions.Reply{Behavior: "maybe"}); !errors.Is(err, decisions.ErrInvalid) {
		t.Errorf("behavior maybe: %v", err)
	}

	for _, c := range []struct {
		name, session, chosen string
		modes                 []string
	}{
		// No mode switches nothing, even with auto first in mode.allow.
		{"definition in plan, no mode", inPlan, "", nil},
		{"definition out of plan, no mode", inAuto, "", nil},
		{"chosen", inPlan, "auto", []string{"auto"}},
		{"repo session, no mode", repo, "", nil},
	} {
		fx = &effects{}
		pl := f.ask(t, c.session, "permission_request", `{"tool":"ExitPlanMode","input":{"plan":"x"}}`)
		if text, err := f.answer(t, fx, c.session, pl, decisions.Reply{Behavior: "allow", Mode: c.chosen}); err != nil || text != `{"behavior":"allow"}` {
			t.Fatalf("%s: %q, %v", c.name, text, err)
		}
		if !slices.Equal(fx.modes, c.modes) || fx.warms != nil {
			t.Errorf("%s: modes %v, want %v; warmed %v", c.name, fx.modes, c.modes, fx.warms)
		}
	}
	fx = &effects{}
	pl := f.ask(t, inPlan, "permission_request", `{"tool":"ExitPlanMode","input":{"plan":"y"}}`)
	if text, err := f.answer(t, fx, inPlan, pl, decisions.Reply{Behavior: "deny", Message: "keep planning"}); err != nil || text != `{"behavior":"deny","message":"keep planning"}` {
		t.Fatalf("plan deny: %q, %v", text, err)
	}
	if fx.modes != nil || fx.warms != nil {
		t.Errorf("plan deny switched %v, warmed %v", fx.modes, fx.warms)
	}
}

// A grantable permission allowed through Answer is a one-time grant: its
// reply names the decision as decisionId, never "grant", which every worker
// adds as a standing rule (worker/src/session.ts, docs/adr/0076 §3).
func TestAnswer_GrantPayloadIsDecisionIdNeverGrant(t *testing.T) {
	f := setup(t)
	s := f.session(t)
	p := f.ask(t, s, "permission_request", `{"tool":"Bash","input":`+bigInput+`}`)
	f.grantable(t, s, p)
	r, _ := f.store.Request(f.ctx, s, p)
	text, err := f.answer(t, &effects{}, s, p, decisions.Reply{Behavior: "allow"})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["grant"]; ok || len(got) != 2 || got["behavior"] != "allow" || got["decisionId"] != r.ID {
		t.Errorf("reply = %s, want exactly behavior and decisionId %s", text, r.ID)
	}
}

// ConsumeGrant: single-use under concurrency, this session's only, and the
// input matched canonically.
func TestConsumeGrant(t *testing.T) {
	f := setup(t)
	s, other := f.session(t), f.session(t)
	p := f.ask(t, s, "permission_request", `{"tool":"Bash","input":`+bigInput+`}`)
	f.grantable(t, s, p)
	r, _ := f.store.Request(f.ctx, s, p)
	if id := f.consume(t, s, "Bash", bigInput); id != "" {
		t.Fatalf("a grant before any answer: %s", id)
	}
	if _, err := f.answer(t, &effects{}, s, p, decisions.Reply{Behavior: "allow"}); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct{ name, session, tool, input string }{
		{"another session", other, "Bash", bigInput},
		{"another tool", s, "Write", bigInput},
		{"a value changed", s, "Bash", `{"command":"ls -l","n":12345678901234567890}`},
		// Equal as float64: Canon keeps numbers as written.
		{"a number changed", s, "Bash", `{"command":"ls -la","n":12345678901234567891}`},
		{"a key added", s, "Bash", `{"command":"ls -la","n":12345678901234567890,"x":1}`},
	} {
		if id := f.consume(t, c.session, c.tool, c.input); id != "" {
			t.Errorf("%s: granted %s", c.name, id)
		}
	}
	if _, err := f.store.ConsumeGrant(f.ctx, s, "Bash", []byte(`{`)); !errors.Is(err, decisions.ErrInvalid) {
		t.Errorf("bad input: %v", err)
	}

	// Key order and spacing are one input; two retries race for one grant.
	same := "{ \"n\": 12345678901234567890,\n  \"command\": \"ls -la\" }"
	var wg sync.WaitGroup
	got := make([]string, 2)
	for i := range got {
		wg.Go(func() {
			id, err := f.store.ConsumeGrant(f.ctx, s, "Bash", []byte(same))
			if err != nil {
				t.Error(err)
			}
			got[i] = id
		})
	}
	wg.Wait()
	slices.Sort(got)
	if got[0] != "" || got[1] != r.ID {
		t.Errorf("two concurrent consumes = %q, want one %s and one none", got, r.ID)
	}
	if id := f.consume(t, s, "Bash", bigInput); id != "" {
		t.Errorf("a spent grant granted again: %s", id)
	}
}

// The race made deterministic: while another consume holds the grant's row
// and then spends it, this one gets nothing, rather than waiting on the lock
// and spending the same grant after it.
func TestConsumeGrant_ALockedGrantIsNotSpentTwice(t *testing.T) {
	f := setup(t)
	s := f.session(t)
	p := f.ask(t, s, "permission_request", `{"tool":"Bash","input":`+bigInput+`}`)
	f.grantable(t, s, p)
	r, _ := f.store.Request(f.ctx, s, p)
	if _, err := f.answer(t, &effects{}, s, p, decisions.Reply{Behavior: "allow"}); err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx) //nolint:errcheck
	if _, err := tx.Exec(f.ctx, `SELECT 1 FROM decisions WHERE id = $1 FOR UPDATE`, r.ID); err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 1)
	go func() {
		id, err := f.store.ConsumeGrant(f.ctx, s, "Bash", []byte(bigInput))
		if err != nil {
			t.Error(err)
		}
		got <- id
	}()
	var id string
	done := false
	select {
	case id = <-got:
		done = true
	case <-time.After(2 * time.Second):
	}
	if _, err := tx.Exec(f.ctx, `UPDATE decisions SET grant_used_at = now() WHERE id = $1`, r.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	if !done {
		id = <-got
	}
	if id != "" {
		t.Errorf("a grant another consume held was spent again: %s", id)
	}
}

// Only a human's allow through Answer mints a grant: never a deny, a
// request that is not grantable, the worker's own resolution (agent or
// worker, even naming the decision), or a human reply Answer did not write.
func TestConsumeGrant_OnlyAnswersMint(t *testing.T) {
	f := setup(t)
	s := f.session(t)
	request := func(grantable bool) (int64, string) {
		seq := f.ask(t, s, "permission_request", `{"tool":"Bash","input":`+bigInput+`}`)
		if grantable {
			f.grantable(t, s, seq)
		}
		r, _ := f.store.Request(f.ctx, s, seq)
		return seq, r.ID
	}
	reply := func(from string, seq int64, text string) {
		if _, err := f.tr.AppendReply(f.ctx, s, from, text, "permission_response", "", seq); err != nil {
			t.Fatal(err)
		}
	}

	seq, _ := request(true)
	if _, err := f.answer(t, &effects{}, s, seq, decisions.Reply{Behavior: "deny"}); err != nil {
		t.Fatal(err)
	}
	seq, _ = request(false)
	if text, err := f.answer(t, &effects{}, s, seq, decisions.Reply{Behavior: "allow"}); err != nil || text != `{"behavior":"allow"}` {
		t.Fatalf("not grantable: %q, %v", text, err)
	}
	for _, from := range []string{"worker", "agent"} {
		seq, id := request(true)
		reply(from, seq, `{"behavior":"allow","decisionId":"`+id+`"}`)
	}
	// A human allow Answer did not write: no decisionId.
	seq, _ = request(true)
	reply("human", seq, `{"behavior":"allow"}`)
	// A human deny naming its decision: OnAppend clears grantable...
	denied, deniedID := request(true)
	reply("human", denied, `{"behavior":"deny","decisionId":"`+deniedID+`"}`)
	var g bool
	if err := f.pool.QueryRow(f.ctx, `SELECT grantable FROM decisions WHERE id = $1`, deniedID).Scan(&g); err != nil || g {
		t.Errorf("a human deny left grantable = %v (%v)", g, err)
	}
	// ...and ConsumeGrant refuses a deny even where grantable survived.
	f.grantable(t, s, denied)

	if id := f.consume(t, s, "Bash", bigInput); id != "" {
		t.Errorf("granted %s", id)
	}
}

// An input with no canon (a lone surrogate escape, which the decoder would
// read as U+FFFD) stores none, so no grant can be minted for it.
func TestConsumeGrant_NoCanonNoGrant(t *testing.T) {
	f := setup(t)
	s := f.session(t)
	const lone = `{"command":"echo \ud800"}`
	p := f.ask(t, s, "permission_request", `{"tool":"Bash","input":`+lone+`}`)
	if r := f.row(t, s, p); r == nil || r.Canon != "" {
		t.Fatalf("row: %+v, want no input_canon", r)
	}
	f.grantable(t, s, p)
	if _, err := f.answer(t, &effects{}, s, p, decisions.Reply{Behavior: "allow"}); err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{lone, `{"command":"echo \ufffd"}`, "{\"command\":\"echo \uFFFD\"}"} {
		if id, _ := f.store.ConsumeGrant(f.ctx, s, "Bash", []byte(in)); id != "" {
			t.Errorf("%s granted %s", in, id)
		}
	}
}

// Archiving ends the session's grants and its open decisions
// (docs/adr/0082 §5): a grant lives until its session ends.
func TestArchive_WithdrawsOpenDecisionsAndUnusedGrants(t *testing.T) {
	f := setup(t)
	s := f.session(t)
	open := f.ask(t, s, "question", `{"questions":[{"question":"which?"}]}`)
	grant := func() int64 {
		seq := f.ask(t, s, "permission_request", `{"tool":"Bash","input":`+bigInput+`}`)
		f.grantable(t, s, seq)
		if _, err := f.answer(t, &effects{}, s, seq, decisions.Reply{Behavior: "allow"}); err != nil {
			t.Fatal(err)
		}
		return seq
	}
	used := grant()
	if id := f.consume(t, s, "Bash", bigInput); id == "" {
		t.Fatal("no grant to spend")
	}
	unused := grant()

	if err := f.sessions.Archive(f.ctx, s); err != nil {
		t.Fatal(err)
	}
	if r := f.row(t, s, open); r.State != "withdrawn" {
		t.Errorf("open question after archive: %s", r.State)
	}
	if id := f.consume(t, s, "Bash", bigInput); id != "" {
		t.Errorf("an archived session's grant spent: %s", id)
	}
	grantable := func(seq int64) bool {
		var g bool
		if err := f.pool.QueryRow(f.ctx, `SELECT grantable FROM decisions WHERE session_id = $1 AND request_seq = $2`, s, seq).Scan(&g); err != nil {
			t.Fatal(err)
		}
		return g
	}
	if grantable(unused) || !grantable(used) {
		t.Errorf("after archive: unused grantable %v (want false), used %v (want true, its record kept)", grantable(unused), grantable(used))
	}
	if r := f.row(t, s, unused); r.State != "answered" {
		t.Errorf("an answered decision rewritten by archive: %s", r.State)
	}
}
