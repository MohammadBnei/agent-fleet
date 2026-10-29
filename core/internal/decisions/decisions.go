// Package decisions is docs/adr/0082 §5's one store of decisions: every
// direction the work waits on the owner for, whatever its kind. A session's
// question, permission request and plan approval are projected here from the
// transcript, in the append's own transaction (OnAppend); a card is the fourth
// kind and arrives with draft_card in a later slice.
package decisions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

// Kind is a decision's kind, as stored.
type Kind string

const (
	Question   Kind = "question"
	Permission Kind = "permission"
	Plan       Kind = "plan"
)

// The states a decision moves through. Expired and stale are a card's
// (docs/adr/0082 §5-6); nothing sets them yet.
const (
	Open      = "open"
	Answered  = "answered"
	Withdrawn = "withdrawn"
	Expired   = "expired"
	Stale     = "stale"
)

// Decision is one kind's reading of its request. Each kind reads its own
// shape; the store keeps what they have in common.
type Decision interface {
	Kind() Kind
	// Ask is what the owner is asked, before clipping to maxAsk.
	Ask() string
}

const maxAsk = 280

// question is AskUserQuestion's {"questions": [{"question", ...}]}
// (sidecar/internal/mcpserver).
type question struct {
	Questions []struct {
		Question string `json:"question"`
	} `json:"questions"`
}

func (question) Kind() Kind { return Question }
func (q question) Ask() string {
	if len(q.Questions) == 0 {
		return ""
	}
	return q.Questions[0].Question
}

// permission is canUseTool's {"tool", "input"} (worker/src/session.ts).
type permission struct {
	Tool  string          `json:"tool"`
	Input json.RawMessage `json:"input"`
}

func (permission) Kind() Kind    { return Permission }
func (p permission) Ask() string { return p.Tool }

// plan is the permission request ExitPlanMode makes; its input is the plan.
type plan struct{ permission }

func (plan) Kind() Kind { return Plan }
func (p plan) Ask() string {
	var in struct {
		Plan string `json:"plan"`
	}
	_ = json.Unmarshal(p.Input, &in)
	return in.Plan
}

// read is the decision an entry of msgType asks, or nil. A request whose
// text does not parse is still a decision of its type: it waits on the owner
// all the same.
func read(msgType, text string) Decision {
	switch msgType {
	case "question":
		var q question
		_ = json.Unmarshal([]byte(text), &q)
		return q
	case "permission_request":
		var p permission
		_ = json.Unmarshal([]byte(text), &p)
		if p.Tool == "ExitPlanMode" {
			return plan{p}
		}
		return p
	}
	return nil
}

// Canon is raw's canonical JSON: object keys sorted, numbers as written
// (UseNumber, so a large integer is not rounded through float64), no
// insignificant space. A one-time grant matches a retried call's input against
// the stored one this way, so both sides must use this function.
func Canon(raw []byte) (string, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return "", err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return "", errors.New("canon: trailing data after the value")
	}
	b, err := json.Marshal(v)
	return string(b), err
}

// asJSON is text for a JSONB column: itself when it is JSON Postgres takes,
// else a JSON string of it. The transcript column already refuses NUL bytes,
// but JSONB also refuses an escaped \u0000, so such text is stored as a string.
func asJSON(text string) string {
	if json.Valid([]byte(text)) && !strings.Contains(text, `\u0000`) {
		return text
	}
	b, _ := json.Marshal(text)
	return string(b)
}

// clip bounds a TEXT column's value read out of JSON: at most n characters,
// and no NUL, which JSON may escape and Postgres TEXT refuses.
func clip(s string, n int) string {
	s = strings.ReplaceAll(s, "\x00", "")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// OnAppend is the transcript's append hook (transcript.NewPostgresStore's
// argument): a reply closes the open decision it names, and a request
// opens one. It runs in the append's transaction, so an error fails the
// append, and the decisions table cannot drift from the transcript it
// replaces as pending_decisions' source.
//
// A reply of any type closes it, as pending_decisions always counted. The
// agent's own reply withdraws rather than answers: that is core's supersede
// of a re-asked question (coreserver reuseOrAppendQuestion).
func OnAppend(ctx context.Context, tx pgx.Tx, sessionID string, e transcript.Entry) error {
	if e.ReplyTo != nil {
		state, by := Answered, e.AuthorName
		if e.From == "agent" {
			state = Withdrawn
		}
		if by == "" {
			by = e.From
		}
		if _, err := tx.Exec(ctx, `
			UPDATE decisions SET state = $3,
			       answer      = CASE WHEN $3 = 'answered' THEN $4::jsonb END,
			       answered_by = CASE WHEN $3 = 'answered' THEN $5 END,
			       answered_at = CASE WHEN $3 = 'answered' THEN now() END,
			       version     = nextval('decisions_version_seq')
			 WHERE session_id = $1 AND request_seq = $2 AND state = 'open'`,
			sessionID, *e.ReplyTo, state, asJSON(e.Text), by); err != nil {
			return err
		}
	}
	d := read(e.Type, e.Text)
	if d == nil {
		return nil
	}
	var tool, canon *string
	var p permission
	switch v := d.(type) {
	case permission:
		p = v
	case plan:
		p = v.permission
	}
	if d.Kind() != Question {
		t := clip(p.Tool, 200)
		tool = &t
		if c, err := Canon(p.Input); err == nil {
			canon = &c
		}
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO decisions (session_id, request_seq, kind, ask, payload, tool, input_canon)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7)`,
		sessionID, e.Seq, string(d.Kind()), clip(d.Ask(), maxAsk), asJSON(e.Text), tool, canon)
	return err
}
