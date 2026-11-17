//go:build integration

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	bridgev1 "github.com/MohammadBnei/ukubi-bridge/proto/gen/go/bridge/v1"
)

// Docs/adr/0084 §5: only a T1 hand-off reaches a character.
func TestCharacterGetsOnlyT1(t *testing.T) {
	d, _, defs, _ := setup(t)
	ctx := context.Background()
	d.SetCards(decisions.NewStore(d.pool), transcript.NewPostgresStore(d.pool, decisions.OnAppend))
	// The warm hook records the resume cursor (MAX(seq)+1) a new pod would get, and fails on demand.
	var warmed []int64
	var warmErr error
	d.SetCharacters(func(ctx context.Context, id string) (string, func(), error) {
		var next int64
		_ = d.pool.QueryRow(ctx, `SELECT COALESCE(MAX(seq) + 1, 0) FROM transcript WHERE session_id = $1`, id).Scan(&next)
		warmed = append(warmed, next)
		return "", func() {}, warmErr
	}, "client/bridge-core")
	defs.heads = append(defs.heads, definitions.Head{Name: "keeper", SHA: "sha", Effective: &definitions.Spec{
		Lifecycle:  &definitions.AllowList{Allow: []string{sessions.Standing}},
		Subscribes: &definitions.Subscribes{Types: []string{"dev.bnei.agent.", "dev.bnei.alert."}},
	}})
	if _, err := d.pool.Exec(ctx, `INSERT INTO agent_definitions (name, sha, valid, effective) VALUES ('keeper', 'sha', true, '{}')`); err != nil {
		t.Fatal(err)
	}
	ss := sessions.NewStore(d.pool)
	char, err := ss.Create(ctx, sessions.CreateParams{DefinitionName: "keeper", DefinitionSHA: "sha", Owner: "client:" + sessions.StandingPrefix + "keeper",
		Visibility: "private", Lifecycle: sessions.Standing, Invoker: sessions.StandingPrefix + "keeper", ThreadKey: "main", TrustSet: "BC", StartedBy: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(set string) string {
		id, err := ss.Create(ctx, sessions.CreateParams{Repo: "notes", TrustSet: set})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	pub := func(ev, session string) {
		if _, err := d.pool.Exec(ctx, `INSERT INTO published_events (session_id, event_id, request_sha) VALUES ($1, $2, 'x')`, session, ev); err != nil {
			t.Fatal(err)
		}
	}
	trusted, tainted := mk("BC"), mk("AB")
	pub("ok", trusted)
	pub("ok2", trusted)
	pub("tainted", tainted)
	hop := func(id, typ string, trust bool, text string) *bridgev1.CloudEvent {
		data, _ := json.Marshal(map[string]string{"text": text})
		return &bridgev1.CloudEvent{Seq: 2, Id: id, Source: "client/bridge-core", Type: typ, Subject: "agent/x",
			Actor: "agent", ActorLogin: "agent:other", ActorTrusted: trust, Data: data}
	}
	n := func() (c int) {
		_ = d.pool.QueryRow(ctx, `SELECT count(*) FROM transcript WHERE session_id = $1`, char).Scan(&c)
		return
	}
	for _, c := range []struct {
		name string
		e    *bridgev1.CloudEvent
		want int
	}{
		{"a publisher holding A", hop("tainted", "dev.bnei.agent.handoff", true, "@keeper hi"), 0},
		{"an unknown publisher", hop("nobody", "dev.bnei.agent.handoff", true, "@keeper hi"), 0},
		{"no cause behind it", hop("ok", "dev.bnei.agent.handoff", false, "@keeper hi"), 0},
		{"not addressed", hop("ok", "dev.bnei.agent.handoff", true, "hi"), 0},
		{"a replayed hand-off", func() *bridgev1.CloudEvent {
			e := hop("ok", "dev.bnei.agent.handoff", true, "@keeper hi")
			e.Replayed = true
			return e
		}(), 0},
		{"a foreign source vouching for a fleet id", func() *bridgev1.CloudEvent {
			e := hop("ok", "dev.bnei.agent.handoff", true, "@keeper hi")
			e.Source = "client/other"
			return e
		}(), 0},
		{"a human GitHub event", func() *bridgev1.CloudEvent {
			e := hop("ok", "dev.bnei.github.issue_comment.created", true, "@keeper hi")
			e.Source, e.Actor, e.ActorLogin = "github.com/MohammadBnei/notes", "human", "MohammadBnei"
			return e
		}(), 0},
		{"a non-agent subscribed type from an agent", hop("ok", "dev.bnei.alert.firing", true, "@keeper hi"), 0},
		{"a T1 hand-off", hop("ok", "dev.bnei.agent.handoff", true, "@keeper hi"), 1},
		{"redelivered", hop("ok", "dev.bnei.agent.handoff", true, "@keeper hi"), 1},
	} {
		if err := d.Handle(ctx, c.e); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := n(); got != c.want {
			t.Fatalf("%s: %d entries, want %d", c.name, got, c.want)
		}
	}
	// The entry landed at or above the cursor the warm saw, and the warm came first.
	if len(warmed) == 0 {
		t.Fatal("the character was never warmed before the append")
	}
	var seq int64
	if err := d.pool.QueryRow(ctx, `SELECT MIN(seq) FROM transcript WHERE session_id = $1`, char).Scan(&seq); err != nil || seq < warmed[0] {
		t.Fatalf("entry seq %d below the warm cursor %v: %v", seq, warmed, err)
	}
	// A failed warm nacks, and the resident delivery still happens.
	warmErr = errors.New("fleet full")
	ev := mention("warmfail")
	ev.Source, ev.Actor, ev.ActorLogin, ev.ActorTrusted = "client/bridge-core", "agent", "agent:other", true
	ev.Type = "dev.bnei.agent.handoff"
	ev.Data = []byte(`{"text":"@keeper @owner hi"}`)
	pub("warmfail", trusted)
	if err := d.Handle(ctx, ev); err == nil || !strings.Contains(err.Error(), "warm character") {
		t.Fatalf("a failed warm must nack, got %v", err)
	}
	warmErr = nil
	if _, err := d.pool.Exec(ctx, `UPDATE sessions SET stopped_at = now() WHERE id = $1`, char); err != nil {
		t.Fatal(err)
	}
	if err := d.Handle(ctx, hop("ok2", "dev.bnei.agent.handoff", true, "@keeper hi")); err != nil || n() != 1 {
		t.Fatalf("a stopped character takes nothing: %v %d", err, n())
	}
}
