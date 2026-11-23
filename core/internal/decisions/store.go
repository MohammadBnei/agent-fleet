package decisions

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound: no decision has that id.
var ErrNotFound = errors.New("decision not found")

// Record is one stored decision, with what its session says about who may
// read it.
type Record struct {
	ID         string
	Kind       Kind
	SessionID  string
	RequestSeq int64
	State      string
	Ask        string
	Payload    string // JSON
	Tool       string
	Answer     string // JSON, "" while unanswered
	AnsweredBy string
	AnsweredAt *time.Time
	Version    int64
	CreatedAt  time.Time
	// Grantable: an allow of this request is a one-time grant (ConsumeGrant).
	Grantable bool
	// NonBlocking: a permission or plan a worker that never blocks asked
	// (its request stamped "nb": 1, docs/adr/0082 §5), read from the stored
	// payload as ReserveSlot and the idle sweep read it.
	//
	// ponytail: a request JSONB refuses (a lone surrogate escape) is stored
	// as a string and reads as unstamped, so it keeps the older behaviour;
	// a stamp column if that ever matters.
	NonBlocking bool
	// Interrupted: open, and an interrupt or abort entry came after the
	// request (derived at read time). The turn that asked has ended.
	// Never a card's: it waits on the owner, not on the turn that drafted it.
	Interrupted bool
	// owner/name, from a card's subject or a thread keyed
	// forge/<owner>/<name>/...; "" otherwise. A card's is lowercase.
	SubjectRepo string
	// A card's: its subject in thread-key form (forge/<owner>/<name>/<kind>/<n>,
	// or forge/<owner>/<name> for a repo), and when it expires.
	Subject   string
	ExpiresAt *time.Time

	// The session's, for the caller's visibility check.
	Owner, Visibility, Definition *string
	Archived                      bool

	// The character the decision belongs to and its thread (docs/adr/0084 amendment A6), from the session row: a
	// standing session's, or a spawned run's, by its principal standing:<name>. Empty for any other session. Core
	// sets them, never the session.
	Character, ThreadKey string
}

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// TurnEndedSQL is true for a decision row d whose asking turn has ended: an
// interrupt or abort came after the request, or, for a permission, the turn's
// result did (the agent heard "pending" and ended its turn on it). Derived at
// read time; no state is written. A question or a plan ends its turn on
// purpose, so a result does not end those; a card is never read this way.
const TurnEndedSQL = `EXISTS (SELECT 1 FROM transcript t
	                              WHERE t.session_id = d.session_id AND t.seq > d.request_seq
	                                AND (t.type IN ('interrupt', 'abort') OR (d.kind = 'permission' AND d.payload->'nb' = '1'::jsonb AND t.type = 'result')))`

const cols = `
	d.id::text, d.kind, d.session_id::text, d.request_seq, d.state, d.ask,
	COALESCE(d.payload::text, ''), COALESCE(d.tool, ''), COALESCE(d.answer::text, ''),
	COALESCE(d.answered_by, ''), d.answered_at, d.version, d.created_at, d.grantable,
	COALESCE(d.payload -> 'nb' = '1'::jsonb, false),
	d.state = 'open' AND d.kind <> 'card' AND ` + TurnEndedSQL + `,
	COALESCE(substring(COALESCE(d.subject, s.thread_key) from '^forge/([^/]+/[^/]+)'), ''),
	COALESCE(d.subject, ''), d.expires_at,
	s.owner, s.visibility, s.definition_name, s.archived_at IS NOT NULL,
	CASE WHEN s.invoker LIKE 'standing:%' THEN substring(s.invoker from 10) ELSE '' END,
	CASE WHEN COALESCE(s.invoker, '') NOT LIKE 'standing:%' THEN ''
	     WHEN s.lifecycle = 'standing' THEN COALESCE((SELECT t.thread_key FROM transcript t
	                                                  WHERE t.session_id = d.session_id AND t.seq = d.request_seq), s.thread_key, '')
	     ELSE COALESCE(s.thread_key, '') END
	FROM decisions d JOIN sessions s ON s.id = d.session_id`

func scan(row pgx.Row) (Record, error) {
	var r Record
	var kind string
	err := row.Scan(&r.ID, &kind, &r.SessionID, &r.RequestSeq, &r.State, &r.Ask,
		&r.Payload, &r.Tool, &r.Answer, &r.AnsweredBy, &r.AnsweredAt, &r.Version, &r.CreatedAt, &r.Grantable,
		&r.NonBlocking, &r.Interrupted, &r.SubjectRepo, &r.Subject, &r.ExpiresAt, &r.Owner, &r.Visibility, &r.Definition, &r.Archived, &r.Character, &r.ThreadKey)
	r.Kind = Kind(kind)
	return r, err
}

func collect(rows pgx.Rows, err error) ([]Record, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Filter is List's query. The audience fields are the store's half of the
// visibility rule: an admin reads the decisions of every open session, since
// the deck is the global sink between agent and human (docs/adr/0082 §5);
// anyone else reads definition sessions only, their own and (Shared) the
// shared ones, as sessions.ListThreads does. The caller still checks each row.
type Filter struct {
	States []string
	Kinds  []Kind
	// Compared ignoring case: GitHub's names are.
	SubjectRepo string
	SessionID   string

	Owner         string
	Shared, Admin bool

	// Before is the page cursor: the last row of the previous page. Nil
	// starts at the newest.
	Before *Cursor
	Limit  int
}

// Cursor is a position in List's order, newest first.
type Cursor struct {
	CreatedAt time.Time
	ID        string
}

// List is decisions on sessions neither archived nor swept, newest first: a
// swept session cannot be resumed, so nothing it asked can be acted on. A
// closed card stays whatever its session became: answering close_threads
// archives the card's own writer, and the owner keeps the record of it.
func (s *Store) List(ctx context.Context, f Filter) ([]Record, error) {
	before := Cursor{CreatedAt: time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), ID: "ffffffff-ffff-ffff-ffff-ffffffffffff"}
	if f.Before != nil {
		before = *f.Before
	}
	kinds := make([]string, len(f.Kinds))
	for i, k := range f.Kinds {
		kinds[i] = string(k)
	}
	if f.States == nil {
		f.States = []string{}
	}
	out, err := collect(s.pool.Query(ctx, `SELECT `+cols+`
		WHERE (s.archived_at IS NULL AND s.swept_at IS NULL OR d.kind = 'card' AND d.state <> 'open')
		  AND ($1 OR (s.definition_name IS NOT NULL AND (s.owner = $2 OR ($3 AND s.visibility = 'shared'))))
		  AND (cardinality($4::text[]) = 0 OR d.state = ANY($4))
		  AND (cardinality($5::text[]) = 0 OR d.kind = ANY($5))
		  AND ($6 = '' OR lower(COALESCE(substring(COALESCE(d.subject, s.thread_key) from '^forge/([^/]+/[^/]+)'), '')) = lower($6))
		  AND ($7 = '' OR d.session_id::text = $7)
		  AND (d.created_at, d.id) < ($8, $9::uuid)
		ORDER BY d.created_at DESC, d.id DESC
		LIMIT $10`,
		f.Admin, f.Owner, f.Shared, f.States, kinds, f.SubjectRepo, f.SessionID,
		before.CreatedAt, before.ID, f.Limit))
	if err != nil {
		return nil, fmt.Errorf("list decisions: %w", err)
	}
	return out, nil
}

// Get is one decision by id, whoever's it is: the caller checks.
func (s *Store) Get(ctx context.Context, id string) (Record, error) {
	r, err := scan(s.pool.QueryRow(ctx, `SELECT `+cols+` WHERE d.id::text = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("get decision: %w", err)
	}
	return r, nil
}

// PollEvery is how often Watch reads the table.
var PollEvery = time.Second

// gapPolls is how many polls a skipped version is looked for again. A version
// is taken from the sequence before its transaction commits, so a lower one
// can become visible after a higher one; a version that never shows up was a
// rolled-back append.
//
// ponytail: an append's transaction open longer than gapPolls×PollEvery is
// missed until the stream's next reconnect, which lists afresh.
const (
	gapPolls = 30
	maxGap   = 100
)

// allStates lets the (state, version) index serve a scan by version alone.
var allStates = []string{Open, Answered, Withdrawn, Expired, Stale}

// Watch calls send with the version to resume from and every decision that
// changed since, polling every PollEvery, until ctx ends or send fails. since
// 0 starts at the current version and sends it first with nothing changed.
func (s *Store) Watch(ctx context.Context, since int64, send func(version int64, changed []Record) error) error {
	last := since
	if since == 0 {
		if err := s.pool.QueryRow(ctx, `SELECT COALESCE(max(version), 0) FROM decisions`).Scan(&last); err != nil {
			return fmt.Errorf("watch decisions: %w", err)
		}
		if err := send(last, nil); err != nil {
			return err
		}
	}
	gaps := map[int64]int{}
	tick := time.NewTicker(PollEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		missing := make([]int64, 0, len(gaps))
		for v := range gaps {
			missing = append(missing, v)
		}
		changed, err := collect(s.pool.Query(ctx, `SELECT `+cols+`
			WHERE d.state = ANY($1) AND (d.version > $2 OR d.version = ANY($3))
			ORDER BY d.version`, allStates, last, missing))
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("watch decisions: %w", err)
		}
		for v, n := range gaps {
			if n <= 1 {
				delete(gaps, v)
			} else {
				gaps[v] = n - 1
			}
		}
		top := last
		for _, r := range changed {
			delete(gaps, r.Version)
			if r.Version > top {
				// Only the versions just below: a version also goes missing
				// when its row changed again, and a resume from long ago
				// would otherwise track every one of those.
				for v := max(top+1, r.Version-maxGap); v < r.Version; v++ {
					gaps[v] = gapPolls
				}
				top = r.Version
			}
		}
		if len(changed) == 0 {
			continue
		}
		last = top
		if err := send(last, changed); err != nil {
			return err
		}
	}
}

// OpenCardsByCharacter counts the open cards tied to each character, keyed by its name: a card on the character's
// own session or a run it spawned (invoker prefix+name), or a core-filed card whose subject is the character
// (subject character/name, such as the budget card). The same rule decisionToProto uses to set Decision.character
// (docs/adr/0084 amendment A3, A6); the invoker wins, so a card is counted once. Sessions archived or swept do not
// count, as List does not show them.
func (s *Store) OpenCardsByCharacter(ctx context.Context, prefix string) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, `SELECT c, count(*)::int FROM (
		SELECT CASE WHEN s.invoker LIKE $1 || '%' THEN substring(s.invoker from char_length($1) + 1)
		            WHEN d.subject LIKE 'character/%' THEN substring(d.subject from 11) END AS c
		FROM decisions d JOIN sessions s ON s.id = d.session_id
		WHERE d.kind = 'card' AND d.state = 'open' AND s.archived_at IS NULL AND s.swept_at IS NULL) x
		WHERE c IS NOT NULL GROUP BY c`, prefix)
	if err != nil {
		return nil, fmt.Errorf("open cards by character: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			return nil, fmt.Errorf("open cards by character: %w", err)
		}
		out[name] = n
	}
	return out, rows.Err()
}
