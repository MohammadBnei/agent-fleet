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
	"log/slog"
	"time"

	"github.com/MohammadBnei/agent-fleet/core/internal/scheduledaudits"
)

// Runner is the thot-facing half, narrowed to what this loop needs so the
// tick logic is testable without a real gRPC connection.
type Runner interface {
	RunAudit(ctx context.Context, auditID, name, prompt string) (string, error)
}

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
		slog.Info("audits: no thot configured, scheduler disabled")
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
		status, err := l.thot.RunAudit(ctx, a.ID, a.Name, a.Prompt)
		if err != nil {
			slog.Error("audits: run", "audit", a.Name, "error", err)
			status = "error: " + err.Error()
		} else {
			slog.Info("audits: ran", "audit", a.Name, "status", status)
		}
		if err := l.store.RecordStatus(ctx, a.ID, status); err != nil {
			slog.Error("audits: record status", "audit", a.Name, "error", err)
		}
	}
}
