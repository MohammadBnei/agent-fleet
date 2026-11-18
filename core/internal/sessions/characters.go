package sessions

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// CharacterThread is one open thread of a character, for the game's list (docs/adr/0084 amendment A3).
type CharacterThread struct {
	Key string
	// Title is the owner's first line in the thread, cut to 60 characters; empty before one.
	Title   string
	Running bool
	LastAt  time.Time
}

// CharacterRuntime is what a started character's row and transcript say about it now.
type CharacterRuntime struct {
	ID, SHA string
	// Stopped is the owner's Stop; PodLive is a pod that holds a slot (pending, created, scheduled or running), and
	// PodRunning one that is up.
	Stopped, PodLive, PodRunning bool
	Draining                     bool
	// Aged: the pod is near its token cap and is replaced through the drain.
	Aged bool
	// Crashes is the crash entries of the last hour plus the failed warms in a row: what backs the re-warm off.
	Crashes    int
	TurnsToday int
	Overridden bool
	Threads    []CharacterThread
	// RunningThread and RunningSince describe the turn running now; RunningThread is empty when none.
	RunningThread string
	RunningSince  time.Time
	// SpawnsLive is the spawned runs holding a pod.
	SpawnsLive int
}

// titleLen is the length a thread title is cut to.
const titleLen = 60

// CharacterRuntime reads one standing row's state for ListCharacters: one row, its open threads and the turn that
// runs. It is a read; nothing here moves the row.
func (s *Store) CharacterRuntime(ctx context.Context, id string) (CharacterRuntime, error) {
	c := CharacterRuntime{ID: id}
	var phase string
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(definition_sha, ''), stopped_at IS NOT NULL, COALESCE(pod_phase, ''),
		drain_started_at IS NOT NULL, COALESCE(pod_started_at < now() - make_interval(secs => $2), false),
		`+crashCount+` + warm_failures,
		(SELECT count(*) FROM sessions o WHERE o.invoker = s.invoker AND o.lifecycle = 'single-shot'
			AND o.pod_phase = ANY($3) AND o.archived_at IS NULL)
		FROM sessions s WHERE s.id = $1`, id, (PodLifetime-ReplaceMargin).Seconds(), livePhases).
		Scan(&c.SHA, &c.Stopped, &phase, &c.Draining, &c.Aged, &c.Crashes, &c.SpawnsLive)
	if err != nil {
		return c, fmt.Errorf("character runtime: %w", err)
	}
	for _, p := range livePhases {
		c.PodLive = c.PodLive || p == phase
	}
	c.PodRunning = phase == "POD_PHASE_RUNNING"
	today, _, over, err := s.Turns(ctx, id)
	if err != nil {
		return c, err
	}
	c.TurnsToday, c.Overridden = today, over
	running, err := s.RunningThreads(ctx, id)
	if err != nil {
		return c, err
	}
	if len(running) > 0 {
		c.RunningThread = running[0]
		// The turn's agent output sits above the thread's last result, so its first entry is when it began.
		if err := s.pool.QueryRow(ctx, `SELECT COALESCE(min(t.created_at), now()) FROM transcript t
			WHERE t.session_id = $1 AND t.thread_key = $2 AND t."from" = 'agent'
			  AND t.seq > COALESCE((SELECT max(r.seq) FROM transcript r WHERE r.session_id = $1 AND r.thread_key = $2 AND r.type = 'result'), -1)`,
			id, c.RunningThread).Scan(&c.RunningSince); err != nil {
			return c, fmt.Errorf("character runtime: running since: %w", err)
		}
	}
	rows, err := s.pool.Query(ctx, `SELECT st.thread_key,
		COALESCE(left(split_part(ltrim(first.text, E' \t\r\n'), E'\n', 1), $2), ''),
		COALESCE(last.at, st.opened_at)
		FROM session_threads st
		LEFT JOIN LATERAL (SELECT h.text FROM transcript h WHERE h.session_id = st.session_id AND h.thread_key = st.thread_key
			AND h."from" = 'human' AND COALESCE(h.type, '') IN ('', 'discussion') ORDER BY h.seq LIMIT 1) first ON true
		LEFT JOIN LATERAL (SELECT max(h.created_at) AS at FROM transcript h WHERE h.session_id = st.session_id
			AND h.thread_key = st.thread_key) last ON true
		WHERE st.session_id = $1 AND st.closed_at IS NULL
		ORDER BY (st.thread_key <> $3), st.opened_at, st.thread_key`, id, titleLen, MainThread)
	if err != nil {
		return c, fmt.Errorf("character runtime: threads: %w", err)
	}
	c.Threads, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (t CharacterThread, err error) {
		err = r.Scan(&t.Key, &t.Title, &t.LastAt)
		return
	})
	if err != nil {
		return c, fmt.Errorf("character runtime: threads: %w", err)
	}
	for i := range c.Threads {
		c.Threads[i].Running = slices.Contains(running, c.Threads[i].Key)
	}
	return c, nil
}
