//go:build integration

package dispatch

import (
	"context"
	"encoding/json"
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
		{"an untrusted type", hop("ok", "dev.bnei.alert.firing", true, "@keeper hi"), 0},
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
	if _, err := d.pool.Exec(ctx, `UPDATE sessions SET stopped_at = now() WHERE id = $1`, char); err != nil {
		t.Fatal(err)
	}
	if err := d.Handle(ctx, hop("ok2", "dev.bnei.agent.handoff", true, "@keeper hi")); err != nil || n() != 1 {
		t.Fatalf("a stopped character takes nothing: %v %d", err, n())
	}
}
