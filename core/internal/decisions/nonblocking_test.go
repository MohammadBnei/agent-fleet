//go:build integration

package decisions_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

// docs/adr/0082 §5, "one decision, never blocking": only a request stamped
// "nb": 1 gets the new behaviour; an older worker's keeps today's.

func (f fixture) grantableAt(t *testing.T, session string, seq int64) bool {
	t.Helper()
	var g bool
	if err := f.pool.QueryRow(f.ctx, `SELECT grantable FROM decisions WHERE session_id = $1 AND request_seq = $2`, session, seq).Scan(&g); err != nil {
		t.Fatal(err)
	}
	return g
}

// Grantable is set at insert: a stamped permission with a canonical input,
// never a plan, never an unstamped request, never one with no canon.
func TestOnAppend_StampedPermissionIsGrantable(t *testing.T) {
	f := setup(t)
	s := f.session(t)
	for _, c := range []struct {
		name, text   string
		want, wantNB bool
	}{
		{"stamped permission", `{"tool":"Bash","input":{"command":"ls"},"nb":1}`, true, true},
		{"stamped plan", `{"tool":"ExitPlanMode","input":{"plan":"p"},"nb":1}`, false, true},
		{"unstamped permission", `{"tool":"Bash","input":{"command":"ls"}}`, false, false},
		// JSONB refuses a lone surrogate, so the payload is stored as a
		// string and reads as unstamped (Record.NonBlocking).
		{"stamped, no canon", `{"tool":"Bash","input":{"command":"\ud800"},"nb":1}`, false, false},
		{"stamped 2", `{"tool":"Bash","input":{"command":"ls"},"nb":2}`, false, false},
	} {
		seq := f.ask(t, s, "permission_request", c.text)
		if got := f.grantableAt(t, s, seq); got != c.want {
			t.Errorf("%s: grantable %v, want %v", c.name, got, c.want)
		}
		r, err := f.store.Request(f.ctx, s, seq)
		if err != nil {
			t.Fatal(err)
		}
		if r.NonBlocking != c.wantNB {
			t.Errorf("%s: NonBlocking %v, want %v", c.name, r.NonBlocking, c.wantNB)
		}
	}
	// End to end: allowed, then spent once by the same call.
	seq := f.ask(t, s, "permission_request", `{"tool":"Bash","input":{"command":"make"},"nb":1}`)
	if _, err := f.answer(t, &effects{}, s, seq, decisions.Reply{Behavior: "allow"}); err != nil {
		t.Fatal(err)
	}
	if id := f.consume(t, s, "Bash", `{ "command" : "make" }`); id == "" {
		t.Error("a stamped allow minted no grant")
	}
	if id := f.consume(t, s, "Bash", `{"command":"make"}`); id != "" {
		t.Error("a grant was spent twice")
	}
}

// A stamped permission or plan warms, then appends; an older worker's does
// not warm (its stale-close would withdraw it).
func TestAnswer_StampedWarmsThenAppends(t *testing.T) {
	f := setup(t)
	s := f.session(t)
	for _, c := range []struct {
		text  string
		reply decisions.Reply
		warm  bool
	}{
		{`{"tool":"Bash","input":{"command":"ls"},"nb":1}`, decisions.Reply{Behavior: "deny", Message: "no"}, true},
		{`{"tool":"ExitPlanMode","input":{"plan":"p"},"nb":1}`, decisions.Reply{Behavior: "allow"}, true},
		{`{"tool":"Bash","input":{"command":"ls"}}`, decisions.Reply{Behavior: "allow"}, false},
		{`{"tool":"ExitPlanMode","input":{"plan":"p"}}`, decisions.Reply{Behavior: "allow"}, false},
	} {
		fx := &effects{tr: f.tr}
		seq := f.ask(t, s, "permission_request", c.text)
		if _, err := f.answer(t, fx, s, seq, c.reply); err != nil {
			t.Fatal(err)
		}
		if got := slices.Equal(fx.warms, []string{s}); got != c.warm {
			t.Errorf("%s: warms %v, want warmed %v", c.text, fx.warms, c.warm)
		}
		// The warm reads RESUME_FROM_SEQ as LatestSeq: it must see the
		// transcript before the answer, or the new pod starts past it.
		if after, _ := f.tr.LatestSeq(f.ctx, s); c.warm && (len(fx.warmedAt) != 1 || fx.warmedAt[0] >= after) {
			t.Errorf("%s: warmed at seq %v, answer at %d; want the warm first", c.text, fx.warmedAt, after)
		}
	}
}

// A warm closes an older worker's permissions and plans, in the transcript and
// here; a stamped one stays open, with no stale reply.
func TestReserveSlot_LeavesStampedRequestsOpen(t *testing.T) {
	f := setup(t)
	id := f.session(t)
	old := f.ask(t, id, "permission_request", `{"tool":"Bash","input":{}}`)
	oldPlan := f.ask(t, id, "permission_request", `{"tool":"ExitPlanMode","input":{}}`)
	nb := f.ask(t, id, "permission_request", `{"tool":"Bash","input":{},"nb":1}`)
	nbPlan := f.ask(t, id, "permission_request", `{"tool":"ExitPlanMode","input":{},"nb":1}`)
	if _, err := f.sessions.ReserveSlot(f.ctx, id, sessions.Caps{MaxLive: 5}); err != nil {
		t.Fatal(err)
	}
	for seq, want := range map[int64]string{old: "withdrawn", oldPlan: "withdrawn", nb: "open", nbPlan: "open"} {
		if r := f.row(t, id, seq); r.State != want {
			t.Errorf("seq %d after warm: %s, want %s", seq, r.State, want)
		}
	}
	var replies []int64
	rows, err := f.pool.Query(f.ctx, `SELECT reply_to_seq FROM transcript WHERE session_id = $1 AND reply_to_seq IS NOT NULL ORDER BY reply_to_seq`, id)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		replies = append(replies, n)
	}
	if !slices.Equal(replies, []int64{old, oldPlan}) {
		t.Errorf("stale replies to %v, want only the older worker's %v", replies, []int64{old, oldPlan})
	}
}

// An older worker's open permission holds its pod against the idle sweep; a
// stamped one does not.
func TestIdleSweep_StampedDoesNotHoldThePod(t *testing.T) {
	f := setup(t)
	idle := func(text string) string {
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
		f.ask(t, id, "permission_request", text)
		return id
	}
	old := idle(`{"tool":"Bash","input":{}}`)
	nb := idle(`{"tool":"Bash","input":{},"nb":1}`)
	nbPlan := idle(`{"tool":"ExitPlanMode","input":{"plan":"p"},"nb":1}`)
	ids, err := f.sessions.ListIdleSessionIDs(f.ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(ids, old) || !slices.Contains(ids, nb) || !slices.Contains(ids, nbPlan) {
		t.Errorf("swept %v: the older worker's pod must stay, the stamped ones may go", ids)
	}
}

// The retention sweep withdraws a session's open decisions and clears its
// unused grants, in MarkSwept's transaction; a spent grant keeps its record.
func TestMarkSwept_WithdrawsDecisions(t *testing.T) {
	f := setup(t)
	s := f.session(t)
	open := f.ask(t, s, "question", `{"questions":[]}`)
	unused := f.ask(t, s, "permission_request", `{"tool":"Bash","input":{"command":"a"},"nb":1}`)
	used := f.ask(t, s, "permission_request", `{"tool":"Bash","input":{"command":"b"},"nb":1}`)
	for _, seq := range []int64{unused, used} {
		if _, err := f.answer(t, &effects{}, s, seq, decisions.Reply{Behavior: "allow"}); err != nil {
			t.Fatal(err)
		}
	}
	f.consume(t, s, "Bash", `{"command":"b"}`)
	if err := f.sessions.MarkSwept(f.ctx, s); err != nil {
		t.Fatal(err)
	}
	if r := f.row(t, s, open); r.State != "withdrawn" {
		t.Errorf("open question after sweep: %s, want withdrawn", r.State)
	}
	if f.grantableAt(t, s, unused) || !f.grantableAt(t, s, used) {
		t.Errorf("after sweep: unused grantable %v (want false), used %v (want true)", f.grantableAt(t, s, unused), f.grantableAt(t, s, used))
	}
	if id := f.consume(t, s, "Bash", `{"command":"a"}`); id != "" {
		t.Error("a swept session's grant was spent")
	}
}

// Interrupted is derived at read time: an open decision with an interrupt or
// abort entry after its request. An earlier one, or a closed decision, is not.
func TestInterrupted(t *testing.T) {
	f := setup(t)
	s := f.session(t)
	f.ask(t, s, "interrupt", "stop")
	before := f.ask(t, s, "permission_request", `{"tool":"Bash","input":{},"nb":1}`)
	answered := f.ask(t, s, "question", `{"questions":[]}`)
	if _, err := f.answer(t, &effects{}, s, answered, decisions.Reply{Answers: `{"answers":{}}`}); err != nil {
		t.Fatal(err)
	}
	interrupted := func(seq int64) bool {
		t.Helper()
		r, err := f.store.Request(f.ctx, s, seq)
		if err != nil {
			t.Fatal(err)
		}
		return r.Interrupted
	}
	if interrupted(before) {
		t.Error("an interrupt before the request marked it interrupted")
	}
	for _, typ := range []string{"interrupt", "abort"} {
		seq := f.ask(t, s, "permission_request", `{"tool":"Bash","input":{"n":"`+typ+`"},"nb":1}`)
		f.ask(t, s, typ, "stop")
		if !interrupted(seq) {
			t.Errorf("%s after the request: not interrupted", typ)
		}
	}
	if !interrupted(before) || interrupted(answered) {
		t.Errorf("open before: %v (want true), answered: %v (want false)", interrupted(before), interrupted(answered))
	}
}

// Withdraw closes an open decision with nothing appended; twice is
// AlreadyAnswered.
func TestWithdraw(t *testing.T) {
	f := setup(t)
	s := f.session(t)
	seq := f.ask(t, s, "permission_request", `{"tool":"Bash","input":{},"nb":1}`)
	r, err := f.store.Request(f.ctx, s, seq)
	if err != nil {
		t.Fatal(err)
	}
	latest, _ := f.tr.LatestSeq(f.ctx, s)
	if err := f.store.Withdraw(f.ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.row(t, s, seq); got.State != "withdrawn" || got.Answer != "" {
		t.Errorf("after withdraw: %+v", got)
	}
	if after, _ := f.tr.LatestSeq(f.ctx, s); after != latest {
		t.Errorf("withdraw appended: latest %d -> %d", latest, after)
	}
	if f.grantableAt(t, s, seq) {
		t.Error("a withdrawn request stayed grantable")
	}
	if err := f.store.Withdraw(f.ctx, r.ID); !errors.Is(err, transcript.ErrAlreadyAnswered) {
		t.Errorf("second withdraw: %v", err)
	}
}

// OpenRequest finds the same stamped call while open; OpenPlans lists open
// plans.
func TestOpenRequestAndPlans(t *testing.T) {
	f := setup(t)
	s := f.session(t)
	f.ask(t, s, "permission_request", `{"tool":"Bash","input":{"command":"ls"}}`) // unstamped: never matched
	seq := f.ask(t, s, "permission_request", `{"tool":"Bash","input":{"command":"ls"},"nb":1}`)
	canon, _ := decisions.Canon([]byte(`{"command":"ls"}`))
	if got, ok, err := f.store.OpenRequest(f.ctx, s, "Bash", canon); err != nil || !ok || got != seq {
		t.Errorf("OpenRequest = %d, %v, %v; want %d", got, ok, err, seq)
	}
	if _, ok, _ := f.store.OpenRequest(f.ctx, s, "Edit", canon); ok {
		t.Error("another tool matched")
	}
	if _, err := f.answer(t, &effects{}, s, seq, decisions.Reply{Behavior: "deny"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := f.store.OpenRequest(f.ctx, s, "Bash", canon); ok {
		t.Error("an answered request matched")
	}
	pl := f.ask(t, s, "permission_request", `{"tool":"ExitPlanMode","input":{"plan":"p"},"nb":1}`)
	if got, err := f.store.OpenPlans(f.ctx, s); err != nil || !slices.Equal(got, []int64{pl}) {
		t.Errorf("OpenPlans = %v, %v; want [%d]", got, err, pl)
	}
}

// Only an older worker's open permission or plan counts as blocking
// (DeriveLiveState's always-Blocked rule); a stamped one and a question do not.
func TestBlockingDecisions_CountsUnstampedOnly(t *testing.T) {
	f := setup(t)
	id := f.session(t)
	f.ask(t, id, "permission_request", `{"tool":"Bash","input":{},"nb":1}`)
	f.ask(t, id, "permission_request", `{"tool":"ExitPlanMode","input":{},"nb":1}`)
	f.ask(t, id, "question", `{"questions":[]}`)
	f.ask(t, id, "permission_request", `{"tool":"Bash","input":{}}`)
	s, err := f.sessions.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if s.PendingDecisions != 4 || s.BlockingDecisions != 1 {
		t.Errorf("pending %d, blocking %d: want 4, 1", s.PendingDecisions, s.BlockingDecisions)
	}
}
