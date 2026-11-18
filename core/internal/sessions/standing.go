package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
)

const (
	// CrashWindow and MaxCrashes: the third crash entry inside the window stops the re-warm (docs/adr/0084 §3).
	CrashWindow = time.Hour
	MaxCrashes  = 3
	// RewarmBackoff is the first back-off; it doubles per crash entry of the window.
	RewarmBackoff = 10 * time.Second
	// MaxBackoff caps the doubling, so a warm that keeps failing is tried hourly, not never and not every tick.
	MaxBackoff = time.Hour
	// DrainDeadline is a thread's turn deadline (15 minutes, docs/adr/0084 §4) plus a minute for the pod to
	// report it: a drain waits no longer than this for a running turn.
	DrainDeadline = 16 * time.Minute
	// PodLifetime is the provisioner's cap on a pod's life, and so on its vault token (grpcserver podLifetime).
	// A character is replaced through the re-pin drain once its pod is older than PodLifetime - ReplaceMargin:
	// the margin is a drain deadline plus a warm, with room for a few missed ticks (docs/adr/0084 §9).
	PodLifetime   = 12 * time.Hour
	ReplaceMargin = 30 * time.Minute
)

// CrashEntryPrefix starts the text of every crash entry core appends for a CRASHED pod: a system entry from
// "fleet", which is never streamed to the agent. They are counted by prefix, so the marshalled form is fixed.
const CrashEntryPrefix = `{"sdk":"pod_crashed"`

// CrashEntry is the text of a crash entry.
func CrashEntry(reason string) string {
	b, _ := json.Marshal(struct {
		SDK    string `json:"sdk"`
		Reason string `json:"reason"`
	}{"pod_crashed", reason})
	return string(b)
}

// crashCount is the crash entries of the last CrashWindow of the session s.
const crashCount = `(SELECT count(*) FROM transcript c WHERE c.session_id = s.id AND c."from" = 'fleet' AND c.type = 'system'
	AND c.text LIKE '{"sdk":"pod_crashed"%' AND c.created_at > now() - interval '1 hour')`

// CrashesInWindow counts a character's crash entries of the last hour.
func (s *Store) CrashesInWindow(ctx context.Context, id string) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT `+crashCount+` FROM sessions s WHERE s.id = $1`, id).Scan(&n); err != nil {
		return 0, fmt.Errorf("crashes in window: %w", err)
	}
	return n, nil
}

// ListRewarmStanding is every started character with no live pod whose re-warm is due (docs/adr/0084 §3): its
// restart_after (NULL is due) plus a back-off that doubles per crash entry of the last hour has passed, and it
// has not crashed MaxCrashes times in that hour. A stopped row is never listed.
func (s *Store) ListRewarmStanding(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT s.id::text FROM sessions s
		CROSS JOIN LATERAL (SELECT `+crashCount+` AS n) c
		WHERE s.lifecycle = 'standing' AND s.stopped_at IS NULL AND s.archived_at IS NULL AND s.swept_at IS NULL
		  AND COALESCE(s.pod_phase, '') <> ALL($1)
		  AND c.n < $3
		  AND COALESCE(s.restart_after, '-infinity') + CASE WHEN c.n + s.warm_failures = 0 THEN interval '0'
		        ELSE make_interval(secs => LEAST($2::float8 * power(2, LEAST(c.n + s.warm_failures, 20) - 1), $4::float8)) END <= now()`,
		livePhases, RewarmBackoff.Seconds(), MaxCrashes, MaxBackoff.Seconds())
	if err != nil {
		return nil, fmt.Errorf("list rewarm standing: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// DeferRestart stamps a character's re-warm clock after a warm that failed, and counts the failure: it backs the
// re-warm off like a crash does (docs/adr/0084 §3), whether the provisioner or core refused the pod.
func (s *Store) DeferRestart(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE sessions SET restart_after = now(), warm_failures = warm_failures + 1 WHERE id = $1 AND lifecycle = 'standing'`, id)
	if err != nil {
		return fmt.Errorf("defer restart: %w", err)
	}
	return nil
}

// WarmSucceeded clears a character's failed-warm count.
func (s *Store) WarmSucceeded(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE sessions SET warm_failures = 0 WHERE id = $1 AND lifecycle = 'standing' AND warm_failures <> 0`, id)
	if err != nil {
		return fmt.Errorf("warm succeeded: %w", err)
	}
	return nil
}

// EndDrain lets intake resume on a character whose drain will not end in a new pod (docs/adr/0084 §3).
func (s *Store) EndDrain(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE sessions SET drain_started_at = NULL WHERE id = $1 AND lifecycle = 'standing' AND drain_started_at IS NOT NULL`, id)
	if err != nil {
		return fmt.Errorf("end drain: %w", err)
	}
	return nil
}

// StandingRow is a started character, for the re-pin sweep.
type StandingRow struct {
	ID, Definition, SHA string
	Live, Draining      bool
	// Aged: the live pod is near its token cap and is replaced, at the same version.
	Aged bool
}

// StandingRows lists the started (not stopped, not archived) characters.
func (s *Store) StandingRows(ctx context.Context) ([]StandingRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT id::text, definition_name, definition_sha, COALESCE(pod_phase, '') = ANY($1),
		drain_started_at IS NOT NULL, COALESCE(pod_started_at < now() - make_interval(secs => $2), false) FROM sessions
		WHERE lifecycle = 'standing' AND stopped_at IS NULL AND archived_at IS NULL AND definition_name IS NOT NULL`,
		livePhases, (PodLifetime - ReplaceMargin).Seconds())
	if err != nil {
		return nil, fmt.Errorf("standing rows: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (c StandingRow, err error) {
		err = r.Scan(&c.ID, &c.Definition, &c.SHA, &c.Live, &c.Draining, &c.Aged)
		return
	})
}

// Draining reports whether a re-pin has stopped intake on the character's threads (docs/adr/0084 §3).
func (s *Store) Draining(ctx context.Context, id string) (bool, error) {
	var d bool
	if err := s.pool.QueryRow(ctx, `SELECT drain_started_at IS NOT NULL FROM sessions WHERE id = $1`, id).Scan(&d); err != nil {
		return false, fmt.Errorf("draining: %w", err)
	}
	return d, nil
}

// Drain stops intake on every thread of a started character and returns when it began, which a later call keeps.
func (s *Store) Drain(ctx context.Context, id string) (time.Time, error) {
	var since time.Time
	err := s.pool.QueryRow(ctx, `UPDATE sessions SET drain_started_at = COALESCE(drain_started_at, now())
		WHERE id = $1 AND lifecycle = 'standing' AND stopped_at IS NULL RETURNING drain_started_at`, id).Scan(&since)
	if err != nil {
		return since, fmt.Errorf("drain: %w", err)
	}
	return since, nil
}

// Repin moves a character to sha and ends its drain (docs/adr/0084 §3).
func (s *Store) Repin(ctx context.Context, id, sha string) error {
	_, err := s.pool.Exec(ctx, `UPDATE sessions SET definition_sha = $2, drain_started_at = NULL, updated_at = now()
		WHERE id = $1 AND lifecycle = 'standing'`, id, sha)
	if err != nil {
		return fmt.Errorf("repin: %w", err)
	}
	return nil
}

// DefinitionEffective is a stored version's effective spec.
func (s *Store) DefinitionEffective(ctx context.Context, name, sha string) (*definitions.Spec, error) {
	var raw []byte
	if err := s.pool.QueryRow(ctx, `SELECT effective FROM agent_definitions WHERE name = $1 AND sha = $2`, name, sha).Scan(&raw); err != nil {
		return nil, fmt.Errorf("definition effective: %w", err)
	}
	var spec definitions.Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, fmt.Errorf("definition effective: %w", err)
	}
	return &spec, nil
}

// StandingHeadFunc is a definition's current version, as definitions.Registry.Resolve tells it.
type StandingHeadFunc func(name string) (definitions.Head, error)

// SetStandingHead wires the registry in for the re-pin sweep. Call before Run. Unset, no character is re-pinned.
func (l *Loop) SetStandingHead(f StandingHeadFunc) { l.standingHead = f }

// SetAlert wires the owner notification: a stalled start, a refused re-pin, a revoked character.
func (l *Loop) SetAlert(f func(ctx context.Context, sessionID, summary string)) { l.alert = f }

// alertOnce tells the owner once per (session, key) until it is forgotten: the sweep runs every tick.
func (l *Loop) alertOnce(ctx context.Context, id, key, summary string) {
	if l.alerted[id][key] {
		return
	}
	if l.alerted[id] == nil {
		l.alerted[id] = map[string]bool{}
	}
	l.alerted[id][key] = true
	slog.Warn("sessions loop: "+summary, "sessionId", id)
	if l.alert != nil {
		l.alert(ctx, id, summary)
	}
}

// forgetRepinAlerts clears what a re-pin or a revocation alerted: its cause is gone. The stall alert is not one of
// them (stalledStanding clears it).
func (l *Loop) forgetRepinAlerts(id string) {
	for k := range l.alerted[id] {
		if k != "stall" {
			delete(l.alerted[id], k)
		}
	}
}

// keepStanding is docs/adr/0084 §3: a started character without a pod is warmed again, and its pin follows its
// definition. Runs before the sweeps, so a drain that tears a pod down re-warms it on the same tick.
func (l *Loop) keepStanding(ctx context.Context) {
	l.repinStanding(ctx)
	f := l.rewarm.Load()
	if f == nil {
		return
	}
	ids, err := l.sessions.ListRewarmStanding(ctx)
	if err != nil {
		slog.Error("sessions loop: list standing rewarm failed", "error", err)
		return
	}
	for _, id := range ids {
		if l.refused[id] {
			continue // its pin is revoked or widened: only Stop and Start moves it
		}
		if err := (*f)(ctx, id); err != nil {
			slog.Warn("sessions loop: standing re-warm failed", "sessionId", id, "error", err)
			if err := l.sessions.DeferRestart(ctx, id); err != nil {
				slog.Error("sessions loop: deferring a standing re-warm failed", "sessionId", id, "error", err)
			}
			continue
		}
		if err := l.sessions.WarmSucceeded(ctx, id); err != nil {
			slog.Error("sessions loop: clearing a standing warm failure count failed", "sessionId", id, "error", err)
		}
		slog.Info("sessions loop: re-warmed a standing character", "sessionId", id)
	}
}

// stalledStanding alerts for a character whose pod came up and said nothing, once per stall: it is not torn down.
func (l *Loop) stalledStanding(ctx context.Context) {
	ids, err := l.sessions.ListStalledStandingIDs(ctx, l.startupStall)
	if err != nil {
		slog.Error("sessions loop: list stalled characters failed", "error", err)
		return
	}
	stalled := map[string]bool{}
	for _, id := range ids {
		stalled[id] = true
		l.alertOnce(ctx, id, "stall", "pod started but never produced any output; a character is not torn down for it")
	}
	// The stall is over (output arrived, or the pod changed): the next one alerts again.
	for id, m := range l.alerted {
		if !stalled[id] {
			delete(m, "stall")
		}
	}
}

func (l *Loop) endDrain(ctx context.Context, id string) {
	if err := l.sessions.EndDrain(ctx, id); err != nil {
		slog.Error("sessions loop: ending a drain failed", "sessionId", id, "error", err)
	}
}

func isStanding(s *definitions.Spec) bool {
	return s != nil && s.Lifecycle != nil && slices.Contains(s.Lifecycle.Allow, Standing)
}

// repinStanding moves each started character to its definition's new version (docs/adr/0084 §3).
//   - Revoked (no valid version, or no longer standing): the pod goes at once.
//   - A change that widens the ceiling, the vault or cluster.read is refused: the owner must Stop and Start. A
//     vault widening also takes the pod down at once.
//   - A narrowing takes the pod down at once and re-pins.
//   - Anything else drains: intake stops on every thread, the running turn gets its deadline, then the Job goes,
//     the row is re-pinned and the re-warm that follows starts the new Job.
func (l *Loop) repinStanding(ctx context.Context) {
	if l.standingHead == nil {
		return
	}
	rows, err := l.sessions.StandingRows(ctx)
	if err != nil {
		slog.Error("sessions loop: standing rows failed", "error", err)
		return
	}
	for _, r := range rows {
		head, err := l.standingHead(r.Definition)
		switch {
		case errors.Is(err, definitions.ErrUnknown), errors.Is(err, definitions.ErrInvalid), err == nil && !isStanding(head.Effective):
			if r.Live {
				l.tearDown(ctx, r.ID)
			}
			l.refused[r.ID] = true
			l.alertOnce(ctx, r.ID, "revoked", "its definition was revoked; Stop it, or fix the definition")
			continue
		case err != nil && r.Live && r.Aged:
			// The registry cannot tell, but the pod is about to hit its token cap: replace it at the version it
			// already runs (the warm checks the pin again), rather than let the Job die undrained at its deadline.
			head = definitions.Head{SHA: r.SHA}
		case err != nil:
			continue // the registry cannot tell: nothing moves
		}
		// The pod is near its token cap: replaced through the same drain, at the same version.
		replace := r.SHA == head.SHA && r.Live && r.Aged
		if r.SHA == head.SHA && !replace {
			if r.Draining {
				// Reverted mid-drain: the pod stays, and intake resumes (what it held is read again from the cursor).
				l.endDrain(ctx, r.ID)
			}
			delete(l.refused, r.ID)
			l.forgetRepinAlerts(r.ID)
			continue
		}
		var narrows, narrowsVault bool
		if !replace {
			old, err := l.sessions.DefinitionEffective(ctx, r.Definition, r.SHA)
			if err != nil {
				slog.Warn("sessions loop: pinned version unreadable, not re-pinning", "sessionId", r.ID, "error", err)
				l.endDrain(ctx, r.ID)
				continue
			}
			if ceiling, vault := definitions.Widens(old, head.Effective); ceiling || vault {
				if r.Draining {
					l.endDrain(ctx, r.ID)
				}
				if vault && r.Live {
					l.tearDown(ctx, r.ID)
				}
				l.refused[r.ID] = true
				l.alertOnce(ctx, r.ID, "widens:"+head.SHA, "its definition widened the ceiling, vault or cluster read; Stop and Start it to take the change")
				continue
			}
			narrows, narrowsVault = definitions.Widens(head.Effective, old)
		}
		if r.Live && !narrows && !narrowsVault {
			since, err := l.sessions.Drain(ctx, r.ID)
			if err != nil {
				slog.Warn("sessions loop: drain failed", "sessionId", r.ID, "error", err)
				continue
			}
			running, err := l.sessions.RunningThreads(ctx, r.ID)
			if err != nil {
				slog.Warn("sessions loop: running threads failed", "sessionId", r.ID, "error", err)
				continue
			}
			if len(running) > 0 && time.Since(since) < DrainDeadline {
				continue
			}
		}
		if r.Live {
			if err := l.provisioner.TearDownWorker(ctx, r.ID); err != nil {
				slog.Warn("sessions loop: re-pin teardown failed", "sessionId", r.ID, "error", err)
				continue
			}
			if err := l.sessions.ClearLease(ctx, r.ID); err != nil {
				slog.Warn("sessions loop: re-pin clearing lease failed", "sessionId", r.ID, "error", err)
			}
		}
		if err := l.sessions.Repin(ctx, r.ID, head.SHA); err != nil {
			slog.Error("sessions loop: re-pin failed", "sessionId", r.ID, "error", err)
			continue
		}
		if err := l.sessions.SetPodPhase(ctx, r.ID, "POD_PHASE_TERMINATED", "re-pinned to a new definition version, or replaced before its token cap"); err != nil {
			slog.Error("sessions loop: re-pin phase failed", "sessionId", r.ID, "error", err)
		}
		delete(l.refused, r.ID)
		l.forgetRepinAlerts(r.ID)
		slog.Info("sessions loop: re-pinned a character in place", "sessionId", r.ID, "from", r.SHA, "to", head.SHA)
	}
}

// PodStartedAt is when the row's current pod was created (SetPodImages stamps it at the warm); the zero time before
// any pod. A Stop's abort entry older than it targeted an earlier pod (docs/adr/0084 amendment A4).
func (s *Store) PodStartedAt(ctx context.Context, id string) (time.Time, error) {
	var at *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT pod_started_at FROM sessions WHERE id = $1`, id).Scan(&at); err != nil {
		return time.Time{}, fmt.Errorf("pod started at: %w", err)
	}
	if at == nil {
		return time.Time{}, nil
	}
	return *at, nil
}
