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
	// owner/name, from a thread keyed github/<owner>/<name>/...; "" otherwise.
	SubjectRepo string

	// The session's, for the caller's visibility check.
	Owner, Visibility, Definition *string
	Archived                      bool
}

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const cols = `
	d.id::text, d.kind, d.session_id::text, d.request_seq, d.state, d.ask,
	COALESCE(d.payload::text, ''), COALESCE(d.tool, ''), COALESCE(d.answer::text, ''),
	COALESCE(d.answered_by, ''), d.answered_at, d.version, d.created_at,
	COALESCE(substring(s.thread_key from '^github/([^/]+/[^/]+)'), ''),
	s.owner, s.visibility, s.definition_name, s.archived_at IS NOT NULL
	FROM decisions d JOIN sessions s ON s.id = d.session_id`

func scan(row pgx.Row) (Record, error) {
	var r Record
	var kind string
	err := row.Scan(&r.ID, &kind, &r.SessionID, &r.RequestSeq, &r.State, &r.Ask,
		&r.Payload, &r.Tool, &r.Answer, &r.AnsweredBy, &r.AnsweredAt, &r.Version, &r.CreatedAt,
		&r.SubjectRepo, &r.Owner, &r.Visibility, &r.Definition, &r.Archived)
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
// visibility rule, as in sessions.ListThreads: an admin reads every
// definition session's decisions, anyone else their own and (Shared) the
// shared ones. The caller still checks each row.
type Filter struct {
	States      []string
	Kinds       []Kind
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

// List is decisions on open definition sessions, newest first.
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
		WHERE s.archived_at IS NULL AND s.definition_name IS NOT NULL
		  AND ($1 OR s.owner = $2 OR ($3 AND s.visibility = 'shared'))
		  AND (cardinality($4::text[]) = 0 OR d.state = ANY($4))
		  AND (cardinality($5::text[]) = 0 OR d.kind = ANY($5))
		  AND ($6 = '' OR COALESCE(substring(s.thread_key from '^github/([^/]+/[^/]+)'), '') = $6)
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
