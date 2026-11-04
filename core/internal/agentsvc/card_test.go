package agentsvc

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/auth"
	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

const cardPayload = `{"ask":"Close them?","subject":{"repo":"o/r","kind":"pr","number":"7"},` +
	`"options":[{"id":"1","label":"Close the threads","effect":{"verb":"close_threads"},"reversible":"no"},` +
	`{"id":"2","label":"Keep them open","effect":{"verb":"handoff"},"reversible":"yes"}],"draftedBy":"core"}`

// The checks before the store: an admin's only, exactly one of option_id
// and own_text, own_text within validMessage's bound.
func TestAnswerCard_Refusals(t *testing.T) {
	admin := Principal{Member: &auth.Identity{Sub: "root", Groups: []string{auth.RequiredGroup}}}
	member := Principal{Member: &auth.Identity{Sub: "alice", Groups: []string{"shared-notes"}}}
	island := Principal{Member: admin.Member, Origin: "https://opus.bnei.dev"}
	r := decisions.Record{ID: "c1", Kind: decisions.Card, State: decisions.Open, Payload: cardPayload}
	for _, tc := range []struct {
		name string
		p    Principal
		m    *agentfleetv1.AnswerDecisionRequest
		code connect.Code
	}{
		{"member", member, &agentfleetv1.AnswerDecisionRequest{OptionId: "1"}, connect.CodePermissionDenied},
		{"admin from an island", island, &agentfleetv1.AnswerDecisionRequest{OptionId: "1"}, connect.CodePermissionDenied},
		{"neither", admin, &agentfleetv1.AnswerDecisionRequest{}, connect.CodeInvalidArgument},
		{"both", admin, &agentfleetv1.AnswerDecisionRequest{OptionId: "1", OwnText: "x"}, connect.CodeInvalidArgument},
		{"own text too long", admin, &agentfleetv1.AnswerDecisionRequest{OwnText: strings.Repeat("x", 2001)}, connect.CodeInvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := (&Server{}).answerCard(context.Background(), tc.p, r, tc.m); connect.CodeOf(err) != tc.code {
				t.Errorf("got %v, want %v", err, tc.code)
			}
		})
	}
}

// docs/adr/0082 §7: from a surface, own words or an option not reversible
// "yes" (or none known) need the confirm page, before any effect; the URL
// names the card's version.
func TestAnswerCard_SurfaceConfirm(t *testing.T) {
	admin := &auth.Identity{Sub: "root", Groups: []string{auth.RequiredGroup}}
	surface := Principal{Member: admin, Origin: "https://game.bnei.dev", Surface: true, fleet: "https://fleet.bnei.dev"}
	partial := strings.Replace(cardPayload, `"reversible":"no"`, `"reversible":"partial"`, 1)
	for _, tc := range []struct {
		name, payload, option, own, want string
	}{
		{"no", cardPayload, "1", "", "https://fleet.bnei.dev/decisions/confirm?id=c1&option=1&v=4"},
		{"partial", partial, "1", "", "option=1&v=4"},
		{"own text", cardPayload, "", "leave it", "option=&own_text=leave+it&v=4"},
		{"unknown option", cardPayload, "9", "", "option=9&v=4"},
	} {
		r := decisions.Record{ID: "c1", Kind: decisions.Card, State: decisions.Open, Payload: tc.payload, Version: 4}
		_, err := (&Server{}).answerCard(context.Background(), surface, r, &agentfleetv1.AnswerDecisionRequest{OptionId: tc.option, OwnText: tc.own})
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || errkeys.Of(err) != errkeys.KeyDecisionConfirmRequired || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v key %q", tc.name, err, errkeys.Of(err))
		}
	}
	member := Principal{Member: &auth.Identity{Sub: "alice"}, Origin: surface.Origin, Surface: true}
	r := decisions.Record{ID: "c1", Kind: decisions.Card, State: decisions.Open, Payload: cardPayload}
	if _, err := (&Server{}).answerCard(context.Background(), member, r, &agentfleetv1.AnswerDecisionRequest{OptionId: "2"}); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a member on a surface: %v", err)
	}
}

// A surface shows an admin cards only, never a permission, plan or question.
func TestVisible_Surface(t *testing.T) {
	admin := Principal{Member: &auth.Identity{Sub: "root", Groups: []string{auth.RequiredGroup}}, Origin: "https://game.bnei.dev", Surface: true}
	member := Principal{Member: &auth.Identity{Sub: "alice"}, Origin: admin.Origin, Surface: true}
	for _, k := range []decisions.Kind{decisions.Card, decisions.Permission, decisions.Plan, decisions.Question} {
		r := decisions.Record{Kind: k, State: decisions.Open}
		if got := (&Server{}).visible(admin, r); got != (k == decisions.Card) {
			t.Errorf("admin on a surface sees a %s: %v", k, got)
		}
		if (&Server{}).visible(member, r) {
			t.Errorf("member on a surface sees a %s", k)
		}
	}
}

func TestCardAnswerErr(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code connect.Code
		key  string
	}{
		{fmt.Errorf("append: %w", decisions.ErrExpired), connect.CodeFailedPrecondition, string(errkeys.KeyDecisionExpired)},
		{fmt.Errorf("append: %w", decisions.ErrStale), connect.CodeFailedPrecondition, string(errkeys.KeyDecisionStale)},
		{decisions.ErrNotOwner, connect.CodePermissionDenied, string(errkeys.KeyInternal)},
		{fmt.Errorf("%w: no option 9", decisions.ErrInvalid), connect.CodeInvalidArgument, ""},
	} {
		err := cardAnswerErr(context.Background(), tc.err)
		if connect.CodeOf(err) != tc.code || string(errkeys.Of(err)) != tc.key {
			t.Errorf("%v: got %v key %q", tc.err, err, errkeys.Of(err))
		}
	}
}

func TestClosesThreads(t *testing.T) {
	r := decisions.Record{Kind: decisions.Card, Payload: cardPayload}
	for _, tc := range []struct {
		option string
		want   bool
	}{{"1", true}, {"2", false}, {"9", false}, {"", false}} {
		if got := closesThreads(r, tc.option); got != tc.want {
			t.Errorf("option %q: %v", tc.option, got)
		}
	}
	if closesThreads(decisions.Record{Kind: decisions.Question, Payload: cardPayload}, "1") {
		t.Error("a question closes threads")
	}
}

// decisionToProto fills Decision.card from the payload, and the writer and
// expiry from the row, which knows them better.
func TestDecisionToProto_Card(t *testing.T) {
	def, exp := "triage", time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	d := decisionToProto(decisions.Record{ID: "c1", Kind: decisions.Card, State: decisions.Open, Payload: cardPayload,
		Definition: &def, ExpiresAt: &exp})
	c := d.GetCard()
	if d.GetKind() != agentfleetv1.DecisionKind_DECISION_KIND_CARD || c.GetAsk() != "Close them?" || len(c.GetOptions()) != 2 ||
		c.GetSubject().GetNumber() != 7 || c.GetDraftedBy() != "core" || c.GetWriterDefinition() != "triage" ||
		c.GetExpiresAt() != "2026-11-06T12:00:00Z" {
		t.Errorf("card: %v", d)
	}
	if decisionToProto(decisions.Record{Kind: decisions.Question, Payload: `{}`}).GetCard() != nil {
		t.Error("a question carries a card")
	}
}
