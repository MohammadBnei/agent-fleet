package decisions

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

var (
	// ErrCardBudget: the writer's definition drafted CardBudget cards in the
	// last CardWindow; this one is refused. Core's own cards are exempt.
	ErrCardBudget = errors.New("card budget spent")
	// ErrExpired and ErrStale: the card is past its expiry, or a newer card
	// on the same subject replaced it. Nothing was done.
	ErrExpired = errors.New("card expired")
	ErrStale   = errors.New("card stale")
	// ErrNotOwner: a reply to a card that a human did not write.
	ErrNotOwner = errors.New("only the owner answers a card")
	// ErrNotDelivered: Answer recorded the owner's answer, and returned its
	// seq alongside this error, but the writer cannot resume to read it
	// (swept, or its definition changed): docs/adr/0082 §5's "not delivered".
	ErrNotDelivered = errors.New("not delivered: the writer cannot resume")
)

// Tunables; tests shrink them.
var (
	// CardTTL: an unanswered card turns expired, no action, after this.
	CardTTL = 7 * 24 * time.Hour
	// CardBudget agent cards per definition within CardWindow.
	CardBudget = 10
	CardWindow = 24 * time.Hour
)

// Draft is a card as its writer says it. Draft fills the rest: option ids,
// the subject's url; the hook fills drafted_by (from From), the writer's
// definition and expires_at.
type Draft struct {
	SessionID string
	// "agent" (CoreService.DraftCard) or "system" (core's own cards).
	From           string
	IdempotencyKey string
	Ask            string
	Subject        *agentfleetv1.CardSubject
	Options        []*agentfleetv1.CardOption
	WriteOwn       bool
}

// Draft validates d, appends it as a CARD entry in its session's transcript
// and returns the decision the append made. A retry with the same
// IdempotencyKey returns the first draft's decision. The hook refuses a
// draft over budget (ErrCardBudget); anything that does not fit a card is
// ErrInvalid.
func (s *Store) Draft(ctx context.Context, tr transcript.Store, d Draft) (Record, error) {
	if d.From != "agent" && d.From != "system" {
		return Record{}, fmt.Errorf("%w: a card is drafted by an agent or by core", ErrInvalid)
	}
	c := proto.Clone(&agentfleetv1.Card{Ask: d.Ask, Subject: d.Subject, Options: d.Options, WriteOwn: d.WriteOwn}).(*agentfleetv1.Card)
	if _, err := normalize(c); err != nil {
		return Record{}, err
	}
	text, err := protojson.Marshal(c)
	if err != nil {
		return Record{}, err
	}
	seq, err := tr.Append(ctx, d.SessionID, d.From, string(text), "card", d.IdempotencyKey)
	if err != nil {
		return Record{}, err
	}
	// Append hands back whatever entry already holds the key, so a key
	// reused from a message or a question is refused, not passed off as
	// this card.
	r, err := s.Request(ctx, d.SessionID, seq)
	if errors.Is(err, ErrNotFound) || err == nil && r.Kind != Card {
		return Record{}, fmt.Errorf("%w: idempotency key %q already names another entry", ErrInvalid, d.IdempotencyKey)
	}
	return r, err
}

// ExpireCards turns every open card past its expiry to expired and returns
// how many it turned. An answer is refused past expiry whether or not this
// ran (OnAppend); it is what moves the row for WatchDecisions.
func (s *Store) ExpireCards(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE decisions SET state = 'expired', version = nextval('decisions_version_seq')
		 WHERE kind = 'card' AND state = 'open' AND expires_at <= now()`)
	if err != nil {
		return 0, fmt.Errorf("expire cards: %w", err)
	}
	return tag.RowsAffected(), nil
}

var repoName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// normalize checks c against docs/adr/0082 §5's card and fills what core
// owns: the option ids and the subject's url. It clears what the hook sets
// (expires_at, writer_definition, drafted_by) and returns the subject in
// thread-key form, the repo lowercased.
func normalize(c *agentfleetv1.Card) (string, error) {
	bad := func(format string, a ...any) (string, error) {
		return "", fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, a...)...)
	}
	if err := text("ask", c.GetAsk(), maxAsk, true); err != nil {
		return "", err
	}
	sub := c.GetSubject()
	if sub == nil {
		return bad("a card needs a subject")
	}
	repo := sub.GetRepo()
	owner, name, _ := strings.Cut(repo, "/")
	if !repoName.MatchString(repo) || owner == "." || owner == ".." || name == "." || name == ".." {
		return bad("subject.repo %q is not owner/name", repo)
	}
	n := strconv.FormatInt(sub.GetNumber(), 10)
	key := "github/" + strings.ToLower(repo)
	switch sub.GetKind() {
	case "pr", "issue", "alert":
		if sub.GetNumber() <= 0 {
			return bad("a %s subject needs its number", sub.GetKind())
		}
		key += "/" + sub.GetKind() + "/" + n
	case "repo":
		if sub.GetNumber() != 0 {
			return bad("a repo subject has no number")
		}
	default:
		return bad(`subject.kind must be "pr", "issue", "alert" or "repo"`)
	}
	sub.Url = map[string]string{
		"pr":    "https://github.com/" + repo + "/pull/" + n,
		"issue": "https://github.com/" + repo + "/issues/" + n,
		"repo":  "https://github.com/" + repo,
	}[sub.GetKind()]

	opts := c.GetOptions()
	if len(opts) < 2 || len(opts) > 3 {
		return bad("a card has two or three options")
	}
	recommended := 0
	for i, o := range opts {
		if o == nil {
			return bad("option %d is empty", i+1)
		}
		if err := text("option label", o.GetLabel(), 80, false); err != nil {
			return "", err
		}
		switch o.GetReversible() {
		case "yes", "partial", "no":
		default:
			return bad(`option %d: reversible must be "yes", "partial" or "no"`, i+1)
		}
		switch o.GetEffect().GetVerb() {
		case "handoff":
		case "close_threads":
			if k := sub.GetKind(); k != "pr" && k != "issue" {
				return bad("option %d: close_threads needs a pr or issue subject", i+1)
			}
			if o.GetReversible() != "no" {
				return bad(`option %d: close_threads is reversible "no"`, i+1)
			}
		default:
			return bad(`option %d: effect.verb must be "handoff" or "close_threads"`, i+1)
		}
		if o.GetRecommended() {
			recommended++
		}
		o.Id = strconv.Itoa(i + 1)
	}
	if recommended > 1 {
		return bad("at most one option is recommended")
	}
	c.ExpiresAt, c.WriterDefinition, c.DraftedBy = "", "", ""
	return key, nil
}

// text checks one of a card's agent-written strings: not blank, at most max
// characters, and no control or format character (bidi overrides, zero-width
// marks) that could make the console show something other than what it
// stores. The ask may break lines.
func text(field, s string, max int, newlines bool) error {
	if !utf8.ValidString(s) || strings.TrimSpace(s) == "" || utf8.RuneCountInString(s) > max {
		return fmt.Errorf("%w: %s must be 1 to %d characters", ErrInvalid, field, max)
	}
	for _, r := range s {
		if (r != '\n' || !newlines) && (unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r)) {
			return fmt.Errorf("%w: %s holds a control or format character (U+%04X)", ErrInvalid, field, r)
		}
	}
	return nil
}

// cardLock is the advisory lock every card draft takes, in the two-key form
// so it never meets the transcript's per-session hashtext(id) locks: class
// 82 (docs/adr/0082), key 0.
//
// ponytail: one fleet-wide lock, fine at ten cards a day per definition;
// lock per definition (budget) and per subject (supersede) if drafts queue.
const cardLock = `SELECT pg_advisory_xact_lock(82, 0)`

// onCard is OnAppend's card branch. It trusts nothing the entry says beyond
// the card itself: only an agent or core writes one, it is checked again as
// Draft checked it, and who drafted it, for which definition, and when it
// expires come from the entry and the session. Under cardLock, so that two
// drafts can neither both pass the budget nor both stay open on one subject:
// an agent's draft past CardBudget fails the append, and the open card on
// the same subject turns stale.
func onCard(ctx context.Context, tx pgx.Tx, sessionID string, e transcript.Entry) error {
	draftedBy := map[string]string{"agent": "agent", "system": "core"}[e.From]
	if draftedBy == "" {
		return fmt.Errorf("%w: a card is written by an agent or by core, not %q", ErrInvalid, e.From)
	}
	var c agentfleetv1.Card
	if err := protojson.Unmarshal([]byte(e.Text), &c); err != nil {
		return fmt.Errorf("%w: a card: %v", ErrInvalid, err)
	}
	subject, err := normalize(&c)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, cardLock); err != nil {
		return fmt.Errorf("card lock: %w", err)
	}
	var def *string
	if err := tx.QueryRow(ctx, `SELECT definition_name FROM sessions WHERE id = $1`, sessionID).Scan(&def); err != nil {
		return fmt.Errorf("card writer: %w", err)
	}
	if draftedBy == "agent" {
		var n int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM decisions d JOIN sessions s ON s.id = d.session_id
			 WHERE d.kind = 'card' AND s.definition_name IS NOT DISTINCT FROM $1
			   AND d.created_at > now() - $2::interval
			   AND d.payload ->> 'draftedBy' IS DISTINCT FROM 'core'`,
			def, CardWindow.String()).Scan(&n); err != nil {
			return fmt.Errorf("card budget: %w", err)
		}
		if n >= CardBudget {
			return fmt.Errorf("%w: %d cards in the last %s", ErrCardBudget, n, CardWindow)
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE decisions SET state = 'stale', version = nextval('decisions_version_seq')
		 WHERE kind = 'card' AND state = 'open' AND lower(subject) = lower($1)`, subject); err != nil {
		return fmt.Errorf("supersede card: %w", err)
	}
	expires := time.Now().UTC().Add(CardTTL).Truncate(time.Second)
	c.ExpiresAt, c.DraftedBy = expires.Format(time.RFC3339), draftedBy
	if def != nil {
		c.WriterDefinition = *def
	}
	payload, err := protojson.Marshal(&c)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO decisions (session_id, request_seq, kind, ask, payload, subject, expires_at)
		VALUES ($1, $2, 'card', $3, $4::jsonb, $5, $6)`,
		sessionID, e.Seq, clip(c.GetAsk(), maxAsk), string(payload), subject, expires)
	return err
}

// cardReply refuses a reply to a card that would not answer it: one a human
// did not write (ErrNotOwner), or one to a card that is stale or past its
// expiry. A reply to anything else, or to a card already closed otherwise,
// is left to OnAppend's ordinary path. FOR UPDATE: ExpireCards or a
// supersede cannot move the row between this read and the answer.
func cardReply(ctx context.Context, tx pgx.Tx, sessionID string, e transcript.Entry) error {
	var state string
	var past bool
	err := tx.QueryRow(ctx, `
		SELECT state, COALESCE(expires_at <= now(), false) FROM decisions
		 WHERE session_id = $1 AND request_seq = $2 AND kind = 'card' FOR UPDATE`,
		sessionID, *e.ReplyTo).Scan(&state, &past)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("card reply: %w", err)
	case e.From != "human":
		return ErrNotOwner
	case state == Stale:
		return ErrStale
	case state == Expired, state == Open && past:
		return ErrExpired
	}
	return nil
}
