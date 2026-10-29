package decisions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
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
// rule (docs/adr/0076 §3).
func response(behavior, message, decisionID string) (string, error) {
	if behavior != "allow" && behavior != "deny" {
		return "", fmt.Errorf(`%w: behavior must be "allow" or "deny"`, ErrInvalid)
	}
	b, err := json.Marshal(struct {
		Behavior   string `json:"behavior"`
		Message    string `json:"message,omitempty"`
		DecisionID string `json:"decisionId,omitempty"`
	}{behavior, message, decisionID})
	return string(b), err
}

// Answer answers an open decision for a human and returns the answer's seq,
// one path per kind (docs/adr/0082 §5). The append's one-reply index still
// decides a race: the loser gets transcript.ErrAlreadyAnswered.
//
// Only a question warms. A permission or a plan is bound to its pod's
// canUseTool promise, and a warm runs ReserveSlot, which withdraws exactly
// those requests: warming would close the decision being answered. They warm
// once that stale-close goes.
func (s *Store) Answer(ctx context.Context, fx Effects, tr transcript.Store, r Record, a Reply) (int64, error) {
	if r.State != Open {
		return 0, transcript.ErrAlreadyAnswered
	}
	seq := r.RequestSeq
	reply := func(text, typ string) (int64, error) {
		return tr.AppendAuthored(ctx, r.SessionID, a.By, text, typ, uuid.NewString(), &seq)
	}
	switch r.Kind {
	case Question:
		// Warm, then append: the next pod streams from above the answer.
		if _, err := fx.WarmIfIdle(ctx, r.SessionID); err != nil {
			return 0, err
		}
		return reply(a.Answers, "answer")
	case Plan:
		text, err := response(a.Behavior, a.Message, "")
		if err != nil {
			return 0, err
		}
		if a.Behavior == "allow" {
			// The mode first: the turn the approval releases runs in it.
			mode, err := s.planExit(ctx, r.SessionID, a.Mode)
			if err != nil {
				return 0, err
			}
			if mode != "" {
				if err := fx.SetMode(ctx, r.SessionID, mode); err != nil {
					return 0, err
				}
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
		text, err := response(a.Behavior, a.Message, id)
		if err != nil {
			return 0, err
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
	}
	return 0, fmt.Errorf("%w: a %s decision is not answered here", ErrInvalid, r.Kind)
}

// planExit is the mode a plan approval switches to: the chosen one, else,
// for a definition session still in plan mode, mode.allow's first mode that
// is not plan. "" switches nothing: a repo session has no mode.allow, and a
// session out of plan mode (the console switches before it answers) stays in
// its mode.
func (s *Store) planExit(ctx context.Context, sessionID, chosen string) (string, error) {
	if chosen != "" {
		return chosen, nil
	}
	var mode string
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE((SELECT m FROM jsonb_array_elements_text(d.effective -> 'mode' -> 'allow') WITH ORDINALITY a (m, i)
		                  WHERE m <> 'plan' ORDER BY i LIMIT 1), '')
		  FROM sessions s
		  JOIN agent_definitions d ON d.name = s.definition_name AND d.sha = s.definition_sha
		 WHERE s.id::text = $1 AND s.permission_mode = 'plan'`, sessionID).Scan(&mode)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("plan exit mode: %w", err)
	}
	return mode, nil
}

// Request is the decision a session's request at seq opened.
func (s *Store) Request(ctx context.Context, sessionID string, seq int64) (Record, error) {
	r, err := scan(s.pool.QueryRow(ctx, `SELECT `+cols+` WHERE d.session_id::text = $1 AND d.request_seq = $2`, sessionID, seq))
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
// writes the row's own id into its reply, and any reply that is not a
// human's clears grantable (OnAppend), so the worker cannot mint its own.
// It has no TTL (owner, 2026-10-29): it lives until the session is archived,
// which clears it (sessions.Store.Archive).
func (s *Store) ConsumeGrant(ctx context.Context, sessionID, tool string, input []byte) (string, error) {
	canon, err := Canon(input)
	if err != nil {
		return "", fmt.Errorf("%w: input_json: %v", ErrInvalid, err)
	}
	var id string
	err = s.pool.QueryRow(ctx, `
		UPDATE decisions SET grant_used_at = now(), version = nextval('decisions_version_seq')
		 WHERE id = (SELECT id FROM decisions
		              WHERE session_id::text = $1 AND tool = $2 AND input_canon = $3
		                AND grantable AND grant_used_at IS NULL AND answer ->> 'decisionId' = id::text
		              ORDER BY request_seq LIMIT 1
		                FOR UPDATE SKIP LOCKED)
		RETURNING id::text`, sessionID, tool, canon).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("consume grant: %w", err)
	}
	return id, nil
}
