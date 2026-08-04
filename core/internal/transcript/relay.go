package transcript

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Notifier posts one transcript entry to its Discord thread. Implemented by
// core/internal/discord (kept as an interface here so transcript
// doesn't import discord — discord imports transcript, not the reverse).
type Notifier interface {
	PostToThread(ctx context.Context, taskID string, e Entry) error
}

const maxRelayAttempts = 5

// RelayLoop retries posting unrelayed planning_transcript entries to
// Discord, so a transient API failure retries instead of (as today's
// unguarded postReply can) crashing the whole watch loop. After
// maxRelayAttempts it marks the row dead-lettered and stops retrying it.
func RelayLoop(ctx context.Context, pool *pgxpool.Pool, notifier Notifier, pollInterval time.Duration) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			relayPending(ctx, pool, notifier)
		}
	}
}

func relayPending(ctx context.Context, pool *pgxpool.Pool, notifier Notifier) {
	// Sidecar-pushed telemetry (docs/adr/0020/0021) rides this same table's
	// idempotency/relay infra but must never reach Discord — raw JSON tool
	// summaries would spam every task thread the moment the sidecar starts
	// using PushToolTelemetry. Flip it to relayed without ever posting,
	// before the SELECT below even sees it — cheap (self-clears every tick),
	// no schema change, and the main query stays untouched.
	if _, err := pool.Exec(ctx, `
		UPDATE planning_transcript SET relayed_to_discord = true
		WHERE relayed_to_discord = false AND type = 'tool_call'
	`); err != nil {
		slog.Error("relay: skip tool_call entries", "error", err)
	}

	rows, err := pool.Query(ctx, `
		SELECT task_id, seq, "from", text, COALESCE(type, '')
		FROM planning_transcript
		WHERE relayed_to_discord = false AND relay_dead_letter = false
		ORDER BY task_id, seq
	`)
	if err != nil {
		slog.Error("relay: query pending", "error", err)
		return
	}
	type pending struct {
		taskID string
		e      Entry
	}
	var items []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.taskID, &p.e.Seq, &p.e.From, &p.e.Text, &p.e.Type); err != nil {
			slog.Error("relay: scan pending", "error", err)
			rows.Close()
			return
		}
		items = append(items, p)
	}
	rows.Close()

	for _, p := range items {
		if err := notifier.PostToThread(ctx, p.taskID, p.e); err != nil {
			slog.Warn("relay: post failed, will retry", "taskId", p.taskID, "seq", p.e.Seq, "error", err)
			_, _ = pool.Exec(ctx, `
				UPDATE planning_transcript
				SET relay_attempts = relay_attempts + 1,
				    relay_last_error = $3,
				    relay_dead_letter = (relay_attempts + 1 >= $4)
				WHERE task_id = $1 AND seq = $2
			`, p.taskID, p.e.Seq, err.Error(), maxRelayAttempts)
			continue
		}
		_, _ = pool.Exec(ctx, `
			UPDATE planning_transcript SET relayed_to_discord = true WHERE task_id = $1 AND seq = $2
		`, p.taskID, p.e.Seq)
	}
}
