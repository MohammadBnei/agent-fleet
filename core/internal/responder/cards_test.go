//go:build integration

package responder

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/remediations"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// docs/adr/0084 §5: the character reads the alert cards and the remediations as typed fields, newest first, from
// since on, and an ordinary card (one a repo session drafted) is not among them.
func TestCards_List(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := sessions.NewStore(pool)
	tr := transcript.NewPostgresStore(pool, decisions.OnAppend)
	ds := decisions.NewStore(pool)
	thread, err := store.Create(ctx, sessions.CreateParams{Repo: "r", PermissionMode: "default"})
	if err != nil {
		t.Fatal(err)
	}
	handoff := func(label string) *agentfleetv1.CardOption {
		return &agentfleetv1.CardOption{Label: label, Reversible: "yes", Effect: &agentfleetv1.CardEffect{Verb: "handoff"}}
	}
	draft := func(key, ask string, sub *agentfleetv1.CardSubject) {
		t.Helper()
		if _, err := ds.Draft(ctx, tr, decisions.Draft{SessionID: thread, From: "system", IdempotencyKey: key, Ask: ask, Subject: sub,
			Options: []*agentfleetv1.CardOption{handoff("You handle it"), handoff("Dismiss")}}); err != nil {
			t.Fatal(err)
		}
	}
	draft("c1", "Alert HighRestarts needs a code fix. restarts=7 deployment/game/api", &agentfleetv1.CardSubject{Kind: "alert_group", GroupKey: "0123456789abcdef"})
	draft("c2", "Not an alert.", &agentfleetv1.CardSubject{Repo: "o/r", Kind: "pr", Number: 4})
	if _, err := remediations.NewStore(pool).Insert(ctx, "responder-a", remediations.Selection{Action: remediations.ActionRestart, Kind: "deployment", Namespace: "game", Name: "api"}, remediations.Bounds{}); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `UPDATE remediations SET created_at = now() - interval '30 minutes'`); err != nil {
		t.Fatal(err)
	}
	got, err := Cards{Pool: pool}.List(ctx, time.Now().Add(-time.Hour), 10)
	if err != nil || len(got) != 2 {
		t.Fatalf("list: %v %v", got, err)
	}
	var card, rem *agentfleetv1.ResponderCard
	for _, c := range got {
		switch c.GetKind() {
		case "card":
			card = c
		case "remediation":
			rem = c
		}
	}
	if card == nil || card.GetState() != "open" || !strings.Contains(card.GetAsk(), "HighRestarts") || card.GetAction() != "" {
		t.Errorf("the alert card: %+v", card)
	}
	if rem == nil || rem.GetState() != "pending" || rem.GetAction() != "restart" || rem.GetTarget() != "game/api" || rem.GetResponder() != "responder-a" || rem.GetAsk() != "" {
		t.Errorf("the remediation: %+v", rem)
	}
	if got[0].GetKind() != "card" || got[1].GetKind() != "remediation" {
		t.Errorf("newest first: %v then %v", got[0], got[1])
	}
	if one, _ := (Cards{Pool: pool}).List(ctx, time.Now().Add(-time.Hour), 1); len(one) != 1 {
		t.Errorf("limit 1: %d", len(one))
	}
	if none, _ := (Cards{Pool: pool}).List(ctx, time.Now().Add(time.Minute), 10); len(none) != 0 {
		t.Errorf("since the future: %d", len(none))
	}
}
