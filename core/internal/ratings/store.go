package ratings

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNoResult: the seq is not a result entry of that session.
var ErrNoResult = errors.New("not a result entry of this session")

// Store is session_ratings (migration 000040) plus the read-only transcript
// queries rating and stats need.
type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// ResultText is the text of the session's result entry at seq.
func (s *Store) ResultText(ctx context.Context, sessionID string, seq int64) (string, error) {
	var text string
	err := s.pool.QueryRow(ctx, `SELECT text FROM transcript WHERE session_id = $1 AND seq = $2 AND type = 'result'`, sessionID, seq).Scan(&text)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNoResult
	}
	return text, err
}

// Results are the session's parsed result entries in seq order.
func (s *Store) Results(ctx context.Context, sessionID string) ([]Result, error) {
	rows, err := s.pool.Query(ctx, `SELECT text FROM transcript WHERE session_id = $1 AND type = 'result' ORDER BY seq`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Result
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return nil, err
		}
		r, _ := ParseResult(text)
		out = append(out, r)
	}
	return out, rows.Err()
}

// LastResult is the session's latest result entry; ok is false when it has none.
func (s *Store) LastResult(ctx context.Context, sessionID string) (r Result, ok bool, err error) {
	var text string
	err = s.pool.QueryRow(ctx, `SELECT text FROM transcript WHERE session_id = $1 AND type = 'result' ORDER BY seq DESC LIMIT 1`, sessionID).Scan(&text)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	r, ok = ParseResult(text)
	return r, ok, nil
}

// RateTurn upserts the thumb (+1/-1) on result entry seq.
func (s *Store) RateTurn(ctx context.Context, sessionID string, seq int64, value int, note, model string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO session_ratings (session_id, seq, kind, value, note, model) VALUES ($1, $2, 'thumb', $3, $4, $5)
		ON CONFLICT (session_id, seq) WHERE kind = 'thumb'
		DO UPDATE SET value = $3, note = $4, model = $5, created_at = now()`, sessionID, seq, value, note, model)
	return err
}

// RateSession upserts the session's one 1..5 score.
func (s *Store) RateSession(ctx context.Context, sessionID string, score int, note, model string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO session_ratings (session_id, kind, value, note, model) VALUES ($1, 'score', $2, $3, $4)
		ON CONFLICT (session_id) WHERE kind = 'score'
		DO UPDATE SET value = $2, note = $3, model = $4, created_at = now()`, sessionID, score, note, model)
	return err
}

// Thumb is one turn's current thumb.
type Thumb struct {
	Seq   int64
	Value int
}

// SessionRatings reads back what RateTurn and RateSession stored for a session; score 0 means none.
func (s *Store) SessionRatings(ctx context.Context, sessionID string) (thumbs []Thumb, score int, note string, err error) {
	rows, err := s.pool.Query(ctx, `SELECT kind, COALESCE(seq, 0), value, note FROM session_ratings WHERE session_id = $1 ORDER BY seq`, sessionID)
	if err != nil {
		return nil, 0, "", err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, n string
		var seq int64
		var v int
		if err := rows.Scan(&kind, &seq, &v, &n); err != nil {
			return nil, 0, "", err
		}
		if kind == "score" {
			score, note = v, n
		} else {
			thumbs = append(thumbs, Thumb{seq, v})
		}
	}
	return thumbs, score, note, rows.Err()
}

// Agg is one model's totals over a window.
type Agg struct {
	Sessions     int64
	InputTokens  int64
	OutputTokens int64
	SDKCostUSD   float64
	ErrorResults int64
	ThumbsUp     int64
	ThumbsDown   int64
	ScoreSum     int64
	Scores       int64
}

// Stats aggregates, per model, the result entries since `since` and the
// ratings made since then. Every result of a session that has one in the
// window is read, so deltas start from the right baseline; only results in
// the window count.
func (s *Store) Stats(ctx context.Context, since time.Time) (map[string]*Agg, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.session_id::text, t.text, t.created_at >= $1
		FROM transcript t
		WHERE t.type = 'result'
		  AND t.session_id IN (SELECT session_id FROM transcript WHERE type = 'result' AND created_at >= $1)
		ORDER BY t.session_id, t.seq`, since)
	if err != nil {
		return nil, fmt.Errorf("stats results: %w", err)
	}
	defer rows.Close()
	type row struct {
		r  Result
		in bool
	}
	bySession := map[string][]row{}
	var order []string
	for rows.Next() {
		var sid, text string
		var in bool
		if err := rows.Scan(&sid, &text, &in); err != nil {
			return nil, err
		}
		r, _ := ParseResult(text)
		if _, seen := bySession[sid]; !seen {
			order = append(order, sid)
		}
		bySession[sid] = append(bySession[sid], row{r, in})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := map[string]*Agg{}
	agg := func(m string) *Agg {
		if m == "" {
			m = "unknown"
		}
		if out[m] == nil {
			out[m] = &Agg{}
		}
		return out[m]
	}
	for _, sid := range order {
		rs := make([]Result, len(bySession[sid]))
		for i, x := range bySession[sid] {
			rs[i] = x.r
		}
		seen := map[string]bool{}
		for i, d := range Deltas(rs) {
			if !bySession[sid][i].in {
				continue
			}
			if rs[i].ModelUsageDelta != nil { // the worker's own per-turn share wins; Deltas covers older results
				d = rs[i].ModelUsageDelta
			}
			for m, u := range d {
				a := agg(m)
				a.InputTokens += u.Prompt()
				a.OutputTokens += u.OutputTokens
				a.SDKCostUSD += u.CostUSD
				if !seen[m] {
					seen[m] = true
					a.Sessions++
				}
			}
			if rs[i].IsError {
				agg(rs[i].Turn()).ErrorResults++
			}
		}
	}

	rr, err := s.pool.Query(ctx, `
		SELECT model, kind, count(*) FILTER (WHERE value > 0 AND kind = 'thumb'),
		       count(*) FILTER (WHERE value < 0 AND kind = 'thumb'), COALESCE(sum(value) FILTER (WHERE kind = 'score'), 0),
		       count(*) FILTER (WHERE kind = 'score')
		FROM session_ratings WHERE created_at >= $1 GROUP BY model, kind`, since)
	if err != nil {
		return nil, fmt.Errorf("stats ratings: %w", err)
	}
	defer rr.Close()
	for rr.Next() {
		var model, kind string
		var up, down, sum, scores int64
		if err := rr.Scan(&model, &kind, &up, &down, &sum, &scores); err != nil {
			return nil, err
		}
		a := agg(model)
		a.ThumbsUp += up
		a.ThumbsDown += down
		a.ScoreSum += sum
		a.Scores += scores
	}
	return out, rr.Err()
}
