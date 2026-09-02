package schedules

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Runner files the proposal a schedule's run lands as. Narrowed to what this
// loop needs so the tick logic is testable without the dashboard.
//
// A schedule files a PROPOSAL, never a session — machine-initiated work has no
// pod path (docs/adr/0048), so a stuck cadence cannot accumulate running
// agents. The dedup key collapses a tick that arrives while the previous run
// is still standing.
type Runner interface {
	// The trailing payload argument is the raw document a proposal was filed
	// from — an Alertmanager alert, for the webhook. A schedule has none, so
	// this loop always passes "".
	Create(ctx context.Context, repo, source, dedupKey, title, body, payloadJSON string) (string, bool, error)

	// StandingFor names the session holding a dedup key, so a skipped tick can
	// say which one rather than only that something is. "" when the standing
	// proposal has not been opened yet.
	StandingFor(ctx context.Context, repo, dedupKey string) (string, error)
}

// proposalSource is the `source` value schedules file under. It is a
// CHECK-constrained enum on the proposals table (widened by migration 000005);
// a value outside it fails every insert, and tick() only records that in
// last_status.
const proposalSource = "schedule"

type Loop struct {
	store  *Store
	runner Runner
	nudge  chan struct{}
}

func NewLoop(store *Store, runner Runner) *Loop {
	return &Loop{store: store, runner: runner, nudge: make(chan struct{}, 1)}
}

// Nudge asks for an immediate tick. Non-blocking: a full buffer already means
// a tick is pending, so dropping the signal loses nothing. Wired to the
// store's SetOnChange so a dashboard edit is picked up at once.
func (l *Loop) Nudge() {
	select {
	case l.nudge <- struct{}{}:
	default:
	}
}

func (l *Loop) Run(ctx context.Context, pollInterval time.Duration) {
	// A nil runner means nowhere to file — the loop would claim due rows and
	// drop them on the floor, advancing the cursor for schedules that never
	// ran. Don't start at all.
	if l.runner == nil {
		slog.Info("schedules: no proposal store configured, scheduler disabled")
		return
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	l.tick(ctx) // tickers don't fire at t=0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.tick(ctx)
		case <-l.nudge:
			l.tick(ctx)
		}
	}
}

func (l *Loop) tick(ctx context.Context) {
	// now is the DATABASE's clock, and every time computation below anchors on
	// it. See ListDue.
	due, now, err := l.store.ListDue(ctx, 10)
	if err != nil {
		slog.Error("schedules: list due", "error", err)
		return
	}
	for _, s := range due {
		// Sequential, not concurrent: these file proposals, so parallelism
		// would buy nothing but goroutines.
		l.fire(ctx, s, now)
	}
}

func (l *Loop) fire(ctx context.Context, s Schedule, now time.Time) {
	// Claim BEFORE filing. The claim is the exactly-once gate; a lost race
	// means another tick (or another core replica) is already filing this one.
	if s.DueAt(now) {
		next, spent, err := nextRun(s, now)
		if err != nil {
			// A cron that cannot fire would otherwise leave the row due on
			// every tick forever. Disable it and say why — it took a human to
			// type it, and it takes a human to fix it.
			slog.Error("schedules: bad cron, disabling", "schedule", s.Name, "error", err)
			if _, cerr := l.store.Claim(ctx, s.ID, s.NextRunAt, s.NextRunAt, true); cerr != nil {
				slog.Error("schedules: disable", "schedule", s.Name, "error", cerr)
			}
			l.recordStatus(ctx, s, "error: "+err.Error())
			return
		}
		won, err := l.store.Claim(ctx, s.ID, s.NextRunAt, next, spent)
		if err != nil {
			slog.Error("schedules: claim", "schedule", s.Name, "error", err)
			return
		}
		if !won {
			return
		}
	} else {
		// Listed only because a human pressed "run now".
		won, err := l.store.ClaimRunNow(ctx, s.ID)
		if err != nil {
			slog.Error("schedules: claim run-now", "schedule", s.Name, "error", err)
			return
		}
		if !won {
			return
		}
	}

	// Keyed on the schedule's own ID, with no timestamp: the dedup index
	// covers only standing rows, so this means "at most one open run per
	// schedule". A tick that arrives while the previous run is still an
	// un-opened proposal (or still running) collapses into it, and the next
	// tick after it finishes or is dismissed files a fresh one. Without this
	// an un-opened schedule would accumulate one row per cadence forever.
	dedupKey := "schedule:" + s.ID
	proposalID, created, err := l.runner.Create(ctx, s.Repo, proposalSource, dedupKey,
		"Scheduled: "+s.Name, s.Prompt, "")
	status := "proposal " + proposalID
	switch {
	case err != nil:
		slog.Error("schedules: file proposal", "schedule", s.Name, "repo", s.Repo, "error", err)
		status = "error: " + err.Error()
	case !created:
		// Recording this honestly matters: claiming "proposal " for one we
		// didn't create would make a permanently-stuck schedule look like it
		// ran every cadence.
		//
		// Naming the holder matters for the same reason. "previous run still
		// open" is true both of a run genuinely in flight and of one that
		// finished weeks ago and was never archived — and the second is a
		// stall a human has to clear. Only the session id tells them apart,
		// and the dashboard already opens on ?session=<id>.
		status = "skipped: previous run still open"
		holder, herr := l.runner.StandingFor(ctx, s.Repo, dedupKey)
		if herr != nil {
			// Not fatal: the skip itself is already decided, and the
			// generic message below is still true — just less useful.
			slog.Warn("schedules: standing proposal lookup failed", "schedule", s.Name, "error", herr)
		} else if holder != "" {
			status = "skipped: waiting on session " + holder
		}
		slog.Info("schedules: previous run still open, skipping",
			"schedule", s.Name, "waitingOnSession", holder)
	default:
		slog.Info("schedules: filed proposal", "schedule", s.Name, "repo", s.Repo, "proposalId", proposalID)
	}
	l.recordStatus(ctx, s, status)
}

func (l *Loop) recordStatus(ctx context.Context, s Schedule, status string) {
	if err := l.store.RecordStatus(ctx, s.ID, status); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("schedules: record status", "schedule", s.Name, "error", err)
	}
}
