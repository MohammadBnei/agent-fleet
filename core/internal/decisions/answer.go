package decisions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// ErrInvalid: the reply does not fit its decision.
var ErrInvalid = errors.New("invalid answer")

// Reply is a human's answer to one decision.
type Reply struct {
	By transcript.Author
	// A permission or a plan: "allow" or "deny", and a message to the agent.
	Behavior, Message string
	// A question: its answers, as JSON.
	Answers string
	// A plan approved: the mode to leave plan mode into.
	Mode string
	// A permission allowed: docs/adr/0076 §3's remembered rule. "" and
	// "once" remember nothing.
	Scope, RuleForm string
	// A card: exactly one of an option's id or the owner's own words.
	OptionID, OwnText string
}

// Effects is what an answer does outside the decisions table. The console
// (*dashboard.Server) does all three; it imports this package, so it is
// handed in.
type Effects interface {
	WarmIfIdle(ctx context.Context, sessionID string) (string, error)
	SetMode(ctx context.Context, sessionID, mode string) error
	// RememberGrant stores the rule decision's allow names and returns the
	// decision carrying core's "grant", with an undo for a failed append.
	RememberGrant(ctx context.Context, sessionID string, seq int64, decision, scope, form string) (string, func(), error)
}

// response is a permission_response's text. decisionID marks a one-time
// grant (ConsumeGrant): never "grant", which every worker reads as a standing
// rule (docs/adr/0076 §3). kind "plan" marks a non-blocking worker's plan
// answer, so the worker tells it from a permission's without guessing; an
// older worker's answers carry none and stay as they were.
func response(behavior, message, decisionID, kind string) (string, error) {
	if behavior != "allow" && behavior != "deny" {
		return "", fmt.Errorf(`%w: behavior must be "allow" or "deny"`, ErrInvalid)
	}
	b, err := json.Marshal(struct {
		Behavior   string `json:"behavior"`
		Message    string `json:"message,omitempty"`
		DecisionID string `json:"decisionId,omitempty"`
		Kind       string `json:"kind,omitempty"`
	}{behavior, message, decisionID, kind})
	return string(b), err
}

// Answer answers an open decision for a human and returns the answer's seq,
// one path per kind (docs/adr/0082 §5). The append's one-reply index still
// decides a race: the loser gets transcript.ErrAlreadyAnswered.
//
// A question warms, then appends, and so does a permission or plan a worker
// that never blocks asked (NonBlocking): its answer is a message to the
// session, whenever it comes, and ReserveSlot leaves such a request open. An
// older worker's permission or plan does not warm: it is bound to its pod's
// canUseTool promise, and the warm's ReserveSlot withdraws exactly those
// requests, so warming would close the decision being answered.
//
// A card answers with an option or the owner's own words (answerCard).
func (s *Store) Answer(ctx context.Context, fx Effects, tr transcript.Store, r Record, a Reply) (int64, error) {
	if r.Kind == Card {
		switch {
		case r.State == Stale:
			return 0, ErrStale
		case r.State == Expired, r.State == Open && r.ExpiresAt != nil && !time.Now().Before(*r.ExpiresAt):
			return 0, ErrExpired
		}
	}
	if r.State != Open {
		return 0, transcript.ErrAlreadyAnswered
	}
	seq := r.RequestSeq
	reply := func(text, typ string) (int64, error) {
		return tr.AppendAuthored(ctx, r.SessionID, a.By, text, typ, uuid.NewString(), &seq)
	}
	// Warm, then append: the next pod streams from above the answer.
	warm := func() error {
		_, err := fx.WarmIfIdle(ctx, r.SessionID)
		return err
	}
	switch r.Kind {
	case Question:
		if err := warm(); err != nil {
			return 0, err
		}
		return reply(a.Answers, "answer")
	case Plan:
		kind := ""
		if r.NonBlocking {
			kind = string(Plan)
		}
		text, err := response(a.Behavior, a.Message, "", kind)
		if err != nil {
			return 0, err
		}
		if r.NonBlocking {
			if err := warm(); err != nil {
				return 0, err
			}
		}
		// A chosen mode first: the turn the approval releases runs in it.
		// No mode switches nothing, as the console's and the islands' plain
		// approval always has.
		if a.Behavior == "allow" && a.Mode != "" {
			if err := fx.SetMode(ctx, r.SessionID, a.Mode); err != nil {
				return 0, err
			}
		}
		return reply(text, "permission_response")
	case Permission:
		// A grantable request allowed is a one-time grant: this row,
		// answered with its own id, until ConsumeGrant spends it.
		id := ""
		if r.Grantable && a.Behavior == "allow" {
			id = r.ID
		}
		text, err := response(a.Behavior, a.Message, id, "")
		if err != nil {
			return 0, err
		}
		if r.NonBlocking {
			if err := warm(); err != nil {
				return 0, err
			}
		}
		undo := func() {}
		if a.Scope != "" && a.Scope != "once" {
			if text, undo, err = fx.RememberGrant(ctx, r.SessionID, seq, text, a.Scope, a.RuleForm); err != nil {
				return 0, err
			}
		}
		n, err := reply(text, "permission_response")
		if err != nil {
			// The answer did not land, so neither does the rule it carried.
			undo()
		}
		return n, err
	case Card:
		return answerCard(ctx, r, a, reply, warm)
	}
	return 0, fmt.Errorf("%w: a %s decision is not answered here", ErrInvalid, r.Kind)
}

// cardAnswer is a card's answer text, a question's {"answers": {ask: ...}}
// that the worker already renders as a turn (worker formatAnswerTurn), plus
// which card and option it answers for the console's history.
type cardAnswer struct {
	Card     string            `json:"card"`
	OptionID string            `json:"optionId,omitempty"`
	OwnText  bool              `json:"ownText,omitempty"`
	Answers  map[string]string `json:"answers"`
}

// answerCard is docs/adr/0082 §5's card answer: exactly one of an option or
// the owner's own words (only on a write_own card). A handoff or own words
// warm the writer, then append, as a question does. A close_threads option
// only appends: AgentService closes the subject's threads after it, so the
// answer lands before the archive withdraws what is open there.
//
// A writer that cannot resume (FailedPrecondition from the warm: swept, or
// its definition moved on) still gets the answer recorded, and Answer
// returns its seq with ErrNotDelivered. No slot (any other warm error) is
// that error, and the card stays open.
func answerCard(ctx context.Context, r Record, a Reply, reply func(text, typ string) (int64, error), warm func() error) (int64, error) {
	if (a.OptionID == "") == (a.OwnText == "") {
		return 0, fmt.Errorf("%w: a card is answered with one option or the owner's own words", ErrInvalid)
	}
	var c agentfleetv1.Card
	if err := protojson.Unmarshal([]byte(r.Payload), &c); err != nil {
		return 0, fmt.Errorf("card %s: %w", r.ID, err)
	}
	ans := cardAnswer{Card: r.ID}
	handoff := true
	if a.OwnText != "" {
		if !c.GetWriteOwn() {
			return 0, fmt.Errorf("%w: this card takes no answer of your own", ErrInvalid)
		}
		ans.OwnText, ans.Answers = true, map[string]string{c.GetAsk(): a.OwnText}
	} else {
		i := slices.IndexFunc(c.GetOptions(), func(o *agentfleetv1.CardOption) bool { return o.GetId() == a.OptionID })
		if i < 0 {
			return 0, fmt.Errorf("%w: card %s has no option %q", ErrInvalid, r.ID, a.OptionID)
		}
		o := c.GetOptions()[i]
		handoff = o.GetEffect().GetVerb() == "handoff"
		ans.OptionID = o.GetId()
		ans.Answers = map[string]string{c.GetAsk(): fmt.Sprintf("owner chose option %s of card %s: %s (agent claim, owner-chosen)", o.GetId(), r.ID, o.GetLabel())}
	}
	text, err := json.Marshal(ans)
	if err != nil {
		return 0, err
	}
	var undelivered error
	if handoff {
		if err := warm(); connect.CodeOf(err) == connect.CodeFailedPrecondition {
			reason := err.Error()
			if ce := new(connect.Error); errors.As(err, &ce) {
				reason = ce.Message()
			}
			undelivered = &NotDelivered{Reason: reason}
		} else if err != nil {
			return 0, err
		}
	}
	n, err := reply(string(text), "answer")
	if err != nil {
		return 0, err
	}
	return n, undelivered
}

// Withdraw closes an open decision unanswered (AnswerDecision's "withdraw"):
// nothing is appended and nothing warms, so nothing reaches the session.
// One that is no longer open is transcript.ErrAlreadyAnswered.
func (s *Store) Withdraw(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE decisions SET state = 'withdrawn', grantable = false, version = nextval('decisions_version_seq')
		 WHERE id::text = $1 AND state = 'open'`, id)
	if err != nil {
		return fmt.Errorf("withdraw decision: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return transcript.ErrAlreadyAnswered
	}
	return nil
}

// OpenRequest is the seq of this session's open non-blocking request for
// this tool and canonical input, if there is one: the same call asked again
// (coreserver SendMessage returns it rather than asking twice).
func (s *Store) OpenRequest(ctx context.Context, sessionID, tool, canon string) (int64, bool, error) {
	session, err := uuid.Parse(sessionID)
	if err != nil {
		return 0, false, nil
	}
	var seq int64
	err = s.pool.QueryRow(ctx, `
		SELECT request_seq FROM decisions
		 WHERE session_id = $1 AND state = 'open' AND kind IN ('permission', 'plan')
		   AND tool = $2 AND input_canon = $3 AND payload -> 'nb' = '1'::jsonb
		 ORDER BY request_seq DESC LIMIT 1`, session, tool, canon).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("open request: %w", err)
	}
	return seq, true, nil
}

// OpenPlans is the request seqs of this session's open plan approvals.
func (s *Store) OpenPlans(ctx context.Context, sessionID string) ([]int64, error) {
	session, err := uuid.Parse(sessionID)
	if err != nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT request_seq FROM decisions
		 WHERE session_id = $1 AND state = 'open' AND kind = 'plan' ORDER BY request_seq`, session)
	if err != nil {
		return nil, fmt.Errorf("open plans: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// Request is the decision a session's request at seq opened.
func (s *Store) Request(ctx context.Context, sessionID string, seq int64) (Record, error) {
	id, err := uuid.Parse(sessionID)
	if err != nil {
		return Record{}, ErrNotFound
	}
	r, err := scan(s.pool.QueryRow(ctx, `SELECT `+cols+` WHERE d.session_id = $1 AND d.request_seq = $2`, id, seq))
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("get decision: %w", err)
	}
	return r, nil
}

// ConsumeGrant spends one unused one-time grant of this session for this
// tool and input, and returns its decision's id, or "" when there is none.
// The input is compared canonically (Canon). SKIP LOCKED: of two concurrent
// retries of one call, one gets the grant and the other none, never both.
//
// A grant is a grantable request a human allowed through Answer: Answer
// writes the row's own id into its reply, and any reply but a human's allow
// clears grantable (OnAppend), so the worker cannot mint its own.
// It has no TTL (owner, 2026-10-29): it lives until the session is archived,
// which clears it (sessions.Store.Archive).
func (s *Store) ConsumeGrant(ctx context.Context, sessionID, tool string, input []byte) (string, error) {
	session, err := uuid.Parse(sessionID)
	if err != nil {
		return "", nil
	}
	canon, err := Canon(input)
	if err != nil {
		return "", fmt.Errorf("%w: input_json: %v", ErrInvalid, err)
	}
	var id string
	err = s.pool.QueryRow(ctx, `
		UPDATE decisions SET grant_used_at = now(), version = nextval('decisions_version_seq')
		 WHERE id = (SELECT id FROM decisions
		              WHERE session_id = $1 AND tool = $2 AND input_canon = $3
		                AND grantable AND grant_used_at IS NULL
		                AND answer ->> 'behavior' = 'allow' AND answer ->> 'decisionId' = id::text
		              ORDER BY request_seq LIMIT 1
		                FOR UPDATE SKIP LOCKED)
		RETURNING id::text`, session, tool, canon).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("consume grant: %w", err)
	}
	return id, nil
}
