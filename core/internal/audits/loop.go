// Package audits fires thot's scheduled cluster checks (docs/adr/0035).
//
// Structurally identical to internal/dispatch's and internal/transcript's
// own loops — a ticker plus a size-1 nudge channel — because that pattern
// is already proven twice in this codebase. It's a separate loop rather
// than a third job inside dispatch.tick(): dispatch is entirely about
// claiming tasks and reconciling worker pods, and audits share none of
// that state.
package audits

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/MohammadBnei/agent-fleet/core/internal/scheduledaudits"
)

// Runner creates the task an audit runs as. Narrowed to what this loop
// needs so the tick logic is testable without a database.
//
// docs/adr/0037: an audit is now just a thot task. It shows up in the
// task list, streams into a transcript, and can be interrupted like
// anything else — instead of being an invisible RPC into a standing
// service.
//
// CreateDeduped rather than CreateTaskOfKind: an audit is machine-created,
// so it lands as a proposal a human approves (that method is the only one
// that produces them), and the external key gives audits the duplicate
// collapsing they previously had none of.
type Runner interface {
	CreateDeduped(ctx context.Context, kind, externalKey, repo, description string, channelID, threadID *string) (string, bool, error)
}

// auditRepo is where a thot session's worktree comes from — the repo that
// holds the cluster's own IaC, so a durable fix is a normal PR (adr/0037).
const auditRepo = "infra-bootstrap"

type Loop struct {
	store *scheduledaudits.Store
	thot  Runner
	nudge chan struct{}
}

func New(store *scheduledaudits.Store, thot Runner) *Loop {
	return &Loop{store: store, thot: thot, nudge: make(chan struct{}, 1)}
}

// Nudge asks for an immediate tick. Non-blocking: a full buffer already
// means a tick is pending, so dropping the signal loses nothing. Wired to
// the store's SetOnChange so a dashboard edit is picked up at once.
func (l *Loop) Nudge() {
	select {
	case l.nudge <- struct{}{}:
	default:
	}
}

func (l *Loop) Run(ctx context.Context, pollInterval time.Duration) {
	// A nil thot client means no thot deployed — the loop would claim due
	// rows and drop them on the floor, advancing next_run_at for audits
	// that never ran. Don't start at all.
	if l.thot == nil {
		slog.Info("audits: no task store configured, scheduler disabled")
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
	due, err := l.store.ClaimDue(ctx, 10)
	if err != nil {
		slog.Error("audits: claim due", "error", err)
		return
	}
	for _, a := range due {
		// Sequential, not concurrent: thot serializes everything onto one
		// session anyway, so firing these in parallel would just queue
		// them up on thot's side while holding more goroutines here.
		// Keyed on the audit's own ID, with no timestamp: the dedup index
		// covers only ACTIVE rows, so this means "at most one open run per
		// audit". A cadence tick that arrives while the previous run is
		// still an un-approved proposal (or still running) collapses into
		// it, and the next tick after it finishes or is dismissed creates
		// a fresh one. Without this an un-approved audit would accumulate
		// one row per cadence forever.
		taskID, created, err := l.thot.CreateDeduped(ctx, "thot", "audit:"+a.ID, auditRepo,
			fmt.Sprintf("Scheduled audit: %s\n\n%s", a.Name, a.Prompt), nil, nil)
		status := "task " + taskID
		switch {
		case err != nil:
			slog.Error("audits: create task", "audit", a.Name, "error", err)
			status = "error: " + err.Error()
		case !created:
			// Recording this honestly matters: claiming "task " for a task
			// we didn't create would make a permanently-stuck audit look
			// like it ran every cadence.
			slog.Info("audits: previous run still open, skipping", "audit", a.Name)
			status = "skipped: previous run still open"
		default:
			slog.Info("audits: proposed task", "audit", a.Name, "taskId", taskID)
		}
		if err := l.store.RecordStatus(ctx, a.ID, status); err != nil {
			slog.Error("audits: record status", "audit", a.Name, "error", err)
		}
	}
}
