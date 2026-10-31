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

func TestCardAnswerErr(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code connect.Code
		key  string
	}{
		{fmt.Errorf("append: %w", decisions.ErrExpired), connect.CodeFailedPrecondition, string(errkeys.KeyDecisionExpired)},
		{fmt.Errorf("append: %w", decisions.ErrStale), connect.CodeFailedPrecondition, string(errkeys.KeyDecisionStale)},
		{decisions.ErrNotOwner, connect.CodePermissionDenied, ""},
		{fmt.Errorf("%w: no option 9", decisions.ErrInvalid), connect.CodeInvalidArgument, ""},
	} {
		err := cardAnswerErr(tc.err)
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
