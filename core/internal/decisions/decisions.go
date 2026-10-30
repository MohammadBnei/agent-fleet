// Package decisions is docs/adr/0082 §5's one store of decisions: every
// direction the work waits on the owner for, whatever its kind. A session's
// question, permission request and plan approval are projected here from the
// transcript, in the append's own transaction (OnAppend); a card, the fourth
// kind, is a CARD entry core writes for draft_card (Store.Draft).
package decisions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

// Kind is a decision's kind, as stored.
type Kind string

const (
	Question   Kind = "question"
	Permission Kind = "permission"
	Plan       Kind = "plan"
	// Card is a decision card (docs/adr/0082 §5): a CARD transcript entry
	// core writes (Store.Draft), its payload the protojson of an
	// agentfleetv1.Card.
	Card Kind = "card"
)

// The states a decision moves through. Expired and stale are a card's
// (docs/adr/0082 §5-6): ExpireCards and a newer card on its subject.
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

// permission is canUseTool's {"tool", "input"} (worker/src/session.ts). A
// worker that never blocks on it stamps "nb": 1 (docs/adr/0082 §5); an older
// one, which an image override keeps running indefinitely, does not, and its
// requests keep the blocking behaviour.
type permission struct {
	Tool  string          `json:"tool"`
	Input json.RawMessage `json:"input"`
	NB    int             `json:"nb"`
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

// NonBlocking reads a permission request a worker that never blocks sent
// (stamped "nb": 1): its tool, its input's Canon ("" when it has none) and
// whether it is a plan. ok is false for any other text.
func NonBlocking(text string) (tool, canon string, isPlan, ok bool) {
	var p permission
	_ = json.Unmarshal([]byte(text), &p)
	if p.NB != 1 {
		return "", "", false, false
	}
	canon, _ = Canon(p.Input)
	return clip(p.Tool, 200), canon, p.Tool == "ExitPlanMode", true
}

// Canon is raw's canonical JSON: object keys sorted, numbers as written
// (UseNumber, so a large integer is not rounded through float64), no
// insignificant space. A one-time grant matches a retried call's input against
// the stored one this way, so both sides must use this function.
//
// Input that is not valid UTF-8, or that escapes a lone surrogate, has no
// canon: the decoder would turn either into U+FFFD, so two different inputs
// would share one. Such a request is never grantable.
func Canon(raw []byte) (string, error) {
	if !utf8.Valid(raw) {
		return "", errors.New("canon: not valid UTF-8")
	}
	if loneSurrogate(raw) {
		return "", errors.New("canon: a lone surrogate escape")
	}
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

// loneSurrogate reports a \uD800-\uDFFF escape that is not a high surrogate
// followed by a low one. A backslash outside a string is not valid JSON, which
// the decoder refuses, so raw can be scanned without tracking strings.
func loneSurrogate(raw []byte) bool {
	hex4 := func(i int) rune {
		if i+4 > len(raw) {
			return -1
		}
		n, err := strconv.ParseUint(string(raw[i:i+4]), 16, 16)
		if err != nil {
			return -1
		}
		return rune(n)
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++ // the escaped byte: a second backslash is skipped here
		if i >= len(raw) || raw[i] != 'u' {
			continue
		}
		r := hex4(i + 1)
		i += 4
		if r < 0xD800 || r > 0xDFFF {
			continue
		}
		if r >= 0xDC00 || i+2 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
			return true
		}
		if lo := hex4(i + 3); lo < 0xDC00 || lo > 0xDFFF {
			return true
		}
		i += 6
	}
	return false
}

// asJSON is text for a JSONB column: itself when it is JSON Postgres takes,
// else a JSON string of it. The transcript column already refuses NUL bytes,
// but JSONB also refuses an escaped \u0000 and a lone surrogate escape, so
// such text is stored as a string.
func asJSON(text string) string {
	if json.Valid([]byte(text)) && !strings.Contains(text, `\u0000`) && !loneSurrogate([]byte(text)) {
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
//
// A stamped permission (not a plan) with a canonical input is grantable from
// its insert: a human's allow of it is a one-time grant (ConsumeGrant). Only
// that allow keeps it so: a session authors only as agent or worker
// (coreserver auth.go), so it cannot answer itself into a grant, and a deny
// is never one.
//
// A card is its own branch (onCard), and only a human answers one, while it
// is open and unexpired (cardReply).
func OnAppend(ctx context.Context, tx pgx.Tx, sessionID string, e transcript.Entry) error {
	if e.ReplyTo != nil {
		if err := cardReply(ctx, tx, sessionID, e); err != nil {
			return err
		}
		var reply struct {
			Behavior string `json:"behavior"`
		}
		_ = json.Unmarshal([]byte(e.Text), &reply)
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
			       grantable   = grantable AND $6,
			       version     = nextval('decisions_version_seq')
			 WHERE session_id = $1 AND request_seq = $2 AND state = 'open'`,
			sessionID, *e.ReplyTo, state, asJSON(e.Text), by, e.From == "human" && reply.Behavior == "allow"); err != nil {
			return err
		}
	}
	if e.Type == string(Card) {
		return onCard(ctx, tx, sessionID, e)
	}
	d := read(e.Type, e.Text)
	if d == nil {
		return nil
	}
	var tool, canon *string
	grantable := false
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
		grantable = d.Kind() == Permission && p.NB == 1 && canon != nil
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO decisions (session_id, request_seq, kind, ask, payload, tool, input_canon, grantable)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8)`,
		sessionID, e.Seq, string(d.Kind()), clip(d.Ask(), maxAsk), asJSON(e.Text), tool, canon, grantable)
	return err
}
