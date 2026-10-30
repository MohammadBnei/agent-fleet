//go:build integration

package decisions_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// docs/adr/0082 §5-6's cards over real Postgres: Draft and its checks, the
// hook's budget and supersede, who may answer, and each answer's effect.

func opt(label, verb, reversible string) *agentfleetv1.CardOption {
	return &agentfleetv1.CardOption{Label: label, Effect: &agentfleetv1.CardEffect{Verb: verb}, Reversible: reversible}
}

func card(session, repo string, n int64) decisions.Draft {
	return decisions.Draft{
		SessionID: session, From: "agent", Ask: "Ship it?",
		Subject: &agentfleetv1.CardSubject{Repo: repo, Kind: "pr", Number: n},
		Options: []*agentfleetv1.CardOption{opt("Ship", "handoff", "partial"), opt("Close the threads", "close_threads", "no")},
	}
}

func (f fixture) draft(t *testing.T, d decisions.Draft) decisions.Record {
	t.Helper()
	r, err := f.store.Draft(f.ctx, f.tr, d)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func payload(t *testing.T, r decisions.Record) *agentfleetv1.Card {
	t.Helper()
	var c agentfleetv1.Card
	if err := protojson.Unmarshal([]byte(r.Payload), &c); err != nil {
		t.Fatal(err)
	}
	return &c
}

func TestDraft_Checks(t *testing.T) {
	f := setup(t)
	s := f.thread(t, "client:bridge", "shared", "github/acme/web/pr/1")
	edit := func(fn func(d *decisions.Draft)) decisions.Draft {
		d := card(s, "acme/web", 1)
		fn(&d)
		return d
	}
	if _, err := f.tr.Append(f.ctx, s, "agent", `{"questions":[]}`, "question", "q-key"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.tr.Append(f.ctx, s, "agent", "hi", "discussion", "m-key"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		d    decisions.Draft
	}{
		{"a question's idempotency key", edit(func(d *decisions.Draft) { d.IdempotencyKey = "q-key" })},
		{"a message's idempotency key", edit(func(d *decisions.Draft) { d.IdempotencyKey = "m-key" })},
		{"from a human", edit(func(d *decisions.Draft) { d.From = "human" })},
		{"no ask", edit(func(d *decisions.Draft) { d.Ask = " " })},
		{"ask too long", edit(func(d *decisions.Draft) { d.Ask = strings.Repeat("a", 281) })},
		{"a bidi override in the ask", edit(func(d *decisions.Draft) { d.Ask = "ok\u202e?" })},
		{"a zero-width space in a label", edit(func(d *decisions.Draft) { d.Options[0].Label = "Sh\u200bip" })},
		{"a newline in a label", edit(func(d *decisions.Draft) { d.Options[0].Label = "Sh\nip" })},
		{"label too long", edit(func(d *decisions.Draft) { d.Options[0].Label = strings.Repeat("a", 81) })},
		{"one option", edit(func(d *decisions.Draft) { d.Options = d.Options[:1] })},
		{"four options", edit(func(d *decisions.Draft) {
			d.Options = append(d.Options, opt("a", "handoff", "yes"), opt("b", "handoff", "yes"))
		})},
		{"two recommended", edit(func(d *decisions.Draft) { d.Options[0].Recommended, d.Options[1].Recommended = true, true })},
		{"unknown verb", edit(func(d *decisions.Draft) { d.Options[0].Effect.Verb = "merge" })},
		{"no effect", edit(func(d *decisions.Draft) { d.Options[0].Effect = nil })},
		{"unknown reversible", edit(func(d *decisions.Draft) { d.Options[0].Reversible = "maybe" })},
		{"close_threads reversible", edit(func(d *decisions.Draft) { d.Options[1].Reversible = "partial" })},
		{"close_threads on a repo", edit(func(d *decisions.Draft) { d.Subject = &agentfleetv1.CardSubject{Repo: "acme/web", Kind: "repo"} })},
		{"no subject", edit(func(d *decisions.Draft) { d.Subject = nil })},
		{"repo not owner/name", edit(func(d *decisions.Draft) { d.Subject.Repo = "acme" })},
		{"repo with a dot-dot", edit(func(d *decisions.Draft) { d.Subject.Repo = "acme/.." })},
		{"a pr without its number", edit(func(d *decisions.Draft) { d.Subject.Number = 0 })},
		{"a repo with a number", edit(func(d *decisions.Draft) {
			d.Subject = &agentfleetv1.CardSubject{Repo: "acme/web", Kind: "repo", Number: 3}
			d.Options[1] = opt("Keep", "handoff", "yes")
		})},
		{"an alert without its number", edit(func(d *decisions.Draft) {
			d.Subject = &agentfleetv1.CardSubject{Repo: "acme/web", Kind: "alert"}
			d.Options[1] = opt("Keep", "handoff", "yes")
		})},
		{"unknown kind", edit(func(d *decisions.Draft) { d.Subject.Kind = "commit" })},
	} {
		if _, err := f.store.Draft(f.ctx, f.tr, c.d); !errors.Is(err, decisions.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", c.name, err)
		}
	}

	// What core fills, whatever the agent sent for it.
	d := card(s, "Acme/Web", 1)
	d.Options[0].Id, d.Options[0].Recommended = "9", true
	before := time.Now()
	r := f.draft(t, d)
	c := payload(t, r)
	if r.Kind != decisions.Card || r.State != decisions.Open || r.Ask != "Ship it?" || r.Subject != "github/acme/web/pr/1" || r.SubjectRepo != "acme/web" {
		t.Errorf("record %+v", r)
	}
	if r.ExpiresAt == nil || r.ExpiresAt.Sub(before) < decisions.CardTTL-time.Minute || r.ExpiresAt.Sub(before) > decisions.CardTTL+time.Minute {
		t.Errorf("expires at %v, want about %s from now", r.ExpiresAt, decisions.CardTTL)
	}
	if c.GetOptions()[0].GetId() != "1" || c.GetOptions()[1].GetId() != "2" || c.GetSubject().GetUrl() != "https://github.com/Acme/Web/pull/1" ||
		c.GetDraftedBy() != "agent" || c.GetWriterDefinition() != "d" || c.GetExpiresAt() != r.ExpiresAt.UTC().Format(time.RFC3339) {
		t.Errorf("payload %v", c)
	}
	if d.Options[0].GetId() != "9" {
		t.Error("Draft changed the caller's options")
	}
	// A retry is the first draft.
	d.IdempotencyKey = "k"
	first := f.draft(t, d)
	if again := f.draft(t, d); again.ID != first.ID {
		t.Errorf("a retry drafted %s, want %s", again.ID, first.ID)
	}
	for kind, url := range map[string]string{"issue": "https://github.com/acme/web/issues/1", "alert": ""} {
		d := card(s, "acme/web", 1)
		d.Subject.Kind = kind
		d.Options[1] = opt("Keep", "handoff", "yes")
		if r := f.draft(t, d); payload(t, r).GetSubject().GetUrl() != url || r.Subject != "github/acme/web/"+kind+"/1" {
			t.Errorf("%s: url %q subject %q", kind, payload(t, r).GetSubject().GetUrl(), r.Subject)
		}
	}
}

// The hook trusts nothing: only an agent or core writes a card, and it is
// checked again, so a forged entry opens nothing.
func TestOnAppend_CardTrustsNothing(t *testing.T) {
	f := setup(t)
	s := f.thread(t, "client:bridge", "shared", "github/acme/web/pr/1")
	good := `{"ask":"Ship?","subject":{"repo":"acme/web","kind":"pr","number":1},` +
		`"options":[{"label":"a","effect":{"verb":"handoff"},"reversible":"yes"},{"label":"b","effect":{"verb":"handoff"},"reversible":"yes"}],` +
		`"draftedBy":"core","expiresAt":"2099-01-01T00:00:00Z","writerDefinition":"admin"}`
	if _, err := f.tr.AppendAuthored(f.ctx, s, human, good, "card", "", nil); !errors.Is(err, decisions.ErrInvalid) {
		t.Errorf("a human's card: %v", err)
	}
	if _, err := f.tr.Append(f.ctx, s, "agent", `{"ask":"Ship?"}`, "card", ""); !errors.Is(err, decisions.ErrInvalid) {
		t.Errorf("an unchecked card: %v", err)
	}
	seq := f.ask(t, s, "card", good)
	r, err := f.store.Request(f.ctx, s, seq)
	if err != nil {
		t.Fatal(err)
	}
	if c := payload(t, r); c.GetDraftedBy() != "agent" || c.GetWriterDefinition() != "d" || strings.HasPrefix(c.GetExpiresAt(), "2099") {
		t.Errorf("the hook kept what the entry claimed: %v", c)
	}
}

// One open card per subject, case aside; an agent's cards are budgeted per
// definition, core's are not; concurrent drafts on one subject leave one open.
func TestOnAppend_SupersedeAndBudget(t *testing.T) {
	f := setup(t)
	defer func(n int) { decisions.CardBudget = n }(decisions.CardBudget)
	decisions.CardBudget = 3
	s := f.thread(t, "client:bridge", "shared", "github/acme/web/pr/1")

	// Core's card first: it neither counts against the agents' budget nor
	// is refused by it.
	sys := card(s, "acme/web", 3)
	sys.From = "system"
	if r := f.draft(t, sys); payload(t, r).GetDraftedBy() != "core" {
		t.Errorf("core's card drafted by %q", payload(t, r).GetDraftedBy())
	}
	old := f.draft(t, card(s, "acme/web", 1))
	other := f.draft(t, card(s, "acme/web", 2))
	cur := f.draft(t, card(s, "ACME/web", 1))
	if r, _ := f.store.Get(f.ctx, old.ID); r.State != decisions.Stale || r.Version <= old.Version {
		t.Errorf("the replaced card: %s v%d (was v%d)", r.State, r.Version, old.Version)
	}
	for _, id := range []string{other.ID, cur.ID} {
		if r, _ := f.store.Get(f.ctx, id); r.State != decisions.Open {
			t.Errorf("card %s is %s", id, r.State)
		}
	}
	// Three agent cards today, the replaced one included: the fourth is refused.
	if _, err := f.store.Draft(f.ctx, f.tr, card(s, "acme/web", 3)); !errors.Is(err, decisions.ErrCardBudget) {
		t.Errorf("over budget: %v", err)
	}
	f.draft(t, sys)
	// Another definition has its own budget.
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO agent_definitions (name, sha, spec, effective, valid) VALUES ('e', 'sha', '{}', '{}', true)`); err != nil {
		t.Fatal(err)
	}
	var e string
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO sessions (repo, definition_name, definition_sha, owner) VALUES ('notes', 'e', 'sha', 'u1') RETURNING id`).Scan(&e); err != nil {
		t.Fatal(err)
	}
	f.draft(t, card(e, "acme/web", 4))

	// Concurrent drafts from different sessions of one definition: the
	// per-session transcript lock does not serialize them, only the cards
	// lock does. One subject keeps one open card, and the budget admits
	// exactly what is left of it.
	sessions := make([]string, 8)
	for i := range sessions {
		if err := f.pool.QueryRow(f.ctx, `INSERT INTO sessions (repo, definition_name, definition_sha, owner) VALUES ('notes', 'e', 'sha', 'u1') RETURNING id`).Scan(&sessions[i]); err != nil {
			t.Fatal(err)
		}
	}
	race := func(subject func(i int) (string, int64)) []error {
		var wg sync.WaitGroup
		errs := make([]error, len(sessions))
		for i, sid := range sessions {
			wg.Add(1)
			go func() {
				defer wg.Done()
				repo, n := subject(i)
				_, errs[i] = f.store.Draft(f.ctx, f.tr, card(sid, repo, n))
			}()
		}
		wg.Wait()
		return errs
	}
	decisions.CardBudget = 100
	for _, err := range race(func(int) (string, int64) { return "acme/api", 9 }) {
		if err != nil {
			t.Fatal(err)
		}
	}
	var open int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM decisions WHERE subject = 'github/acme/api/pr/9' AND state = 'open'`).Scan(&open); err != nil || open != 1 {
		t.Errorf("open cards on one subject after concurrent drafts: %d, %v", open, err)
	}
	var used int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM decisions d JOIN sessions s ON s.id = d.session_id WHERE d.kind = 'card' AND s.definition_name = 'e'`).Scan(&used); err != nil {
		t.Fatal(err)
	}
	decisions.CardBudget = used + 3
	ok := 0
	for _, err := range race(func(i int) (string, int64) { return "acme/api", int64(100 + i) }) {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, decisions.ErrCardBudget):
			t.Fatal(err)
		}
	}
	if ok != 3 {
		t.Errorf("concurrent drafts admitted over a budget with 3 left: %d", ok)
	}
}

// warmer answers every warm with err.
type warmer struct {
	effects
	err error
}

func (w *warmer) WarmIfIdle(ctx context.Context, id string) (string, error) {
	w.warms = append(w.warms, id)
	return "", w.err
}

func (f fixture) answerCard(t *testing.T, fx decisions.Effects, id string, a decisions.Reply) (int64, error) {
	t.Helper()
	r, err := f.store.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	a.By = human
	return f.store.Answer(f.ctx, fx, f.tr, r, a)
}

func (f fixture) entry(t *testing.T, session string, seq int64) transcript.Entry {
	t.Helper()
	e, _, err := f.tr.ReadSince(f.ctx, session, seq, 1)
	if err != nil || len(e) != 1 {
		t.Fatalf("entry %d: %v %v", seq, e, err)
	}
	return e[0]
}

func TestAnswer_Card(t *testing.T) {
	f := setup(t)
	s := f.thread(t, "client:bridge", "shared", "github/acme/web/pr/1")
	answers := func(text string) map[string]any {
		var v map[string]any
		if err := json.Unmarshal([]byte(text), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	// A handoff warms, then appends the fixed template.
	fx := &effects{tr: f.tr}
	r := f.draft(t, card(s, "acme/web", 1))
	n, err := f.answerCard(t, fx, r.ID, decisions.Reply{OptionID: "1"})
	if err != nil {
		t.Fatal(err)
	}
	e := f.entry(t, s, n)
	want := map[string]any{"card": r.ID, "optionId": "1", "answers": map[string]any{
		"Ship it?": "owner chose option 1 of card " + r.ID + ": Ship (agent claim, owner-chosen)"}}
	if got := answers(e.Text); !jsonEqual(got, want) || e.Type != "answer" || e.ReplyTo == nil || *e.ReplyTo != r.RequestSeq {
		t.Errorf("handoff answer %+v, want %v", e, want)
	}
	if !slices.Equal(fx.warms, []string{s}) || fx.warmedAt[0] != n {
		t.Errorf("warms %v at %v, want one before the answer at %d", fx.warms, fx.warmedAt, n)
	}
	if got, _ := f.store.Get(f.ctx, r.ID); got.State != decisions.Answered || got.AnsweredBy != human.Name {
		t.Errorf("answered card: %+v", got)
	}
	if _, err := f.answerCard(t, fx, r.ID, decisions.Reply{OptionID: "2"}); !errors.Is(err, transcript.ErrAlreadyAnswered) {
		t.Errorf("a second answer: %v", err)
	}

	// close_threads only appends: AgentService closes the threads.
	fx = &effects{}
	r = f.draft(t, card(s, "acme/web", 2))
	if _, err := f.answerCard(t, fx, r.ID, decisions.Reply{OptionID: "2"}); err != nil || fx.warms != nil {
		t.Errorf("close_threads: %v, warms %v", err, fx.warms)
	}
	// It archives the card's own writer, and History still shows it.
	if _, err := f.pool.Exec(f.ctx, `UPDATE sessions SET archived_at = now() WHERE id = $1`, s); err != nil {
		t.Fatal(err)
	}
	if rows, err := f.store.List(f.ctx, decisions.Filter{Admin: true, States: []string{"answered"}, Kinds: []decisions.Kind{decisions.Card}, Limit: 10}); err != nil || !slices.ContainsFunc(rows, func(x decisions.Record) bool { return x.ID == r.ID }) {
		t.Errorf("an answered close card on an archived writer: %v, %d rows", err, len(rows))
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE sessions SET archived_at = NULL WHERE id = $1`, s); err != nil {
		t.Fatal(err)
	}

	// Own words only on a write_own card, and one of the two only.
	r = f.draft(t, card(s, "acme/web", 3))
	for _, a := range []decisions.Reply{{OwnText: "wait"}, {}, {OptionID: "1", OwnText: "wait"}, {OptionID: "7"}} {
		if _, err := f.answerCard(t, fx, r.ID, a); !errors.Is(err, decisions.ErrInvalid) {
			t.Errorf("%+v: %v, want ErrInvalid", a, err)
		}
	}
	d := card(s, "acme/web", 3)
	d.WriteOwn = true
	r = f.draft(t, d)
	n, err = f.answerCard(t, fx, r.ID, decisions.Reply{OwnText: "wait for CI"})
	if err != nil {
		t.Fatal(err)
	}
	if got := answers(f.entry(t, s, n).Text); !jsonEqual(got, map[string]any{"card": r.ID, "ownText": true, "answers": map[string]any{"Ship it?": "wait for CI"}}) {
		t.Errorf("own text answer %v", got)
	}

	// A writer that cannot resume: recorded, not delivered. No slot: an
	// error, and the card stays open.
	gone := &warmer{err: connect.NewError(connect.CodeFailedPrecondition, errors.New("swept"))}
	r = f.draft(t, card(s, "acme/web", 4))
	n, err = f.answerCard(t, gone, r.ID, decisions.Reply{OptionID: "1"})
	if !errors.Is(err, decisions.ErrNotDelivered) || n == 0 {
		t.Errorf("undeliverable: %d, %v", n, err)
	}
	if got, _ := f.store.Get(f.ctx, r.ID); got.State != decisions.Answered {
		t.Errorf("undeliverable card is %s, want answered", got.State)
	}
	busy := &warmer{err: connect.NewError(connect.CodeResourceExhausted, errors.New("at capacity"))}
	r = f.draft(t, card(s, "acme/web", 5))
	if _, err := f.answerCard(t, busy, r.ID, decisions.Reply{OptionID: "1"}); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Errorf("at capacity: %v", err)
	}
	if got, _ := f.store.Get(f.ctx, r.ID); got.State != decisions.Open {
		t.Errorf("a busy answer left the card %s", got.State)
	}

	// Stale, expired, or past expiry before the sweep: nothing is appended.
	stale := f.draft(t, card(s, "acme/web", 6))
	f.draft(t, card(s, "acme/web", 6))
	past := f.draft(t, card(s, "acme/web", 7))
	if _, err := f.pool.Exec(f.ctx, `UPDATE decisions SET expires_at = now() - interval '1 second' WHERE id = $1`, past.ID); err != nil {
		t.Fatal(err)
	}
	latest, _ := f.tr.LatestSeq(f.ctx, s)
	if _, err := f.answerCard(t, fx, stale.ID, decisions.Reply{OptionID: "1"}); !errors.Is(err, decisions.ErrStale) {
		t.Errorf("stale: %v", err)
	}
	if _, err := f.answerCard(t, fx, past.ID, decisions.Reply{OptionID: "1"}); !errors.Is(err, decisions.ErrExpired) {
		t.Errorf("past expiry: %v", err)
	}
	// The hook refuses the same, whoever skips Answer's own check.
	for id, want := range map[int64]error{stale.RequestSeq: decisions.ErrStale, past.RequestSeq: decisions.ErrExpired} {
		if _, err := f.tr.AppendAuthored(f.ctx, s, human, `{}`, "answer", "", &id); !errors.Is(err, want) {
			t.Errorf("a raw answer to %d: %v, want %v", id, err, want)
		}
	}
	if now, _ := f.tr.LatestSeq(f.ctx, s); now != latest {
		t.Errorf("a refused answer appended: %d -> %d", latest, now)
	}
	expired, err := f.store.ExpireCards(f.ctx)
	if err != nil || expired != 1 {
		t.Errorf("ExpireCards: %d, %v", expired, err)
	}
	if got, _ := f.store.Get(f.ctx, past.ID); got.State != decisions.Expired || got.Version <= past.Version {
		t.Errorf("expired card %s v%d (was v%d)", got.State, got.Version, past.Version)
	}
	if _, err := f.answerCard(t, fx, past.ID, decisions.Reply{OptionID: "1"}); !errors.Is(err, decisions.ErrExpired) {
		t.Errorf("expired: %v", err)
	}

	// Only a human answers: the agent's or worker's reply is refused, not a withdraw.
	r = f.draft(t, card(s, "acme/web", 8))
	for _, from := range []string{"agent", "worker", "system"} {
		if _, err := f.tr.AppendReply(f.ctx, s, from, `{}`, "answer", "", r.RequestSeq); !errors.Is(err, decisions.ErrNotOwner) {
			t.Errorf("%s's reply: %v, want ErrNotOwner", from, err)
		}
	}

	// Concurrent answers: one lands.
	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = f.answerCard(t, &effects{}, r.ID, decisions.Reply{OptionID: "1"})
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, transcript.ErrAlreadyAnswered):
			t.Errorf("a concurrent answer: %v", err)
		}
	}
	if ok != 1 {
		t.Errorf("%d concurrent answers landed, want 1", ok)
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// A card never reads as interrupted: it waits on the owner, not on the turn.
func TestCard_NeverInterrupted(t *testing.T) {
	f := setup(t)
	s := f.thread(t, "client:bridge", "shared", "github/acme/web/pr/1")
	r := f.draft(t, card(s, "acme/web", 1))
	f.ask(t, s, "interrupt", "")
	if got, _ := f.store.Get(f.ctx, r.ID); got.Interrupted {
		t.Error("a card read as interrupted")
	}
	if rows, err := f.store.List(f.ctx, decisions.Filter{Admin: true, SubjectRepo: "ACME/Web", Kinds: []decisions.Kind{decisions.Card}, Limit: 10}); err != nil || len(rows) != 1 {
		t.Errorf("List by subject repo: %v, %v", rows, err)
	}
}

// Down, then up again, with a card in the tables.
func TestCardMigration(t *testing.T) {
	f := setup(t)
	s := f.thread(t, "client:bridge", "shared", "github/acme/web/pr/1")
	r := f.draft(t, card(s, "acme/web", 1))
	if _, err := f.pool.Exec(f.ctx, migration(t, "000025_cards.down.sql")); err != nil {
		t.Fatalf("down: %v", err)
	}
	var typ *string
	if err := f.pool.QueryRow(f.ctx, `SELECT type FROM transcript WHERE session_id = $1 AND seq = $2`, s, r.RequestSeq).Scan(&typ); err != nil || typ != nil {
		t.Errorf("the card entry after down: %v, %v", typ, err)
	}
	if _, err := f.pool.Exec(f.ctx, migration(t, "000025_cards.up.sql")); err != nil {
		t.Fatalf("up: %v", err)
	}
}
