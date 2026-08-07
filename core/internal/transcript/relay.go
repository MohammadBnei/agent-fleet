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

// Relay retries posting unrelayed transcript entries to Discord,
// so a transient API failure retries instead of (as today's unguarded
// postReply can) crashing the whole watch loop. After maxRelayAttempts it
// marks the row dead-lettered and stops retrying it.
type Relay struct {
	pool     *pgxpool.Pool
	notifier Notifier
	nudge    chan struct{}
}

func NewRelay(pool *pgxpool.Pool, notifier Notifier) *Relay {
	return &Relay{pool: pool, notifier: notifier, nudge: make(chan struct{}, 1)}
}

// Nudge triggers an immediate relay pass instead of waiting for the next
// tick (reliability-findings.md #5) — non-blocking, safe to call from any
// goroutine (e.g. PostgresStore.Append right after a commit). The ticker
// in Run stays as a fallback in case a nudge is ever missed.
func (r *Relay) Nudge() {
	select {
	case r.nudge <- struct{}{}:
	default:
	}
}

func (r *Relay) Run(ctx context.Context, pollInterval time.Duration) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			relayPending(ctx, r.pool, r.notifier)
		case <-r.nudge:
			relayPending(ctx, r.pool, r.notifier)
		}
	}
}

// discordSafeTypes is an allowlist, not a denylist (reliability-
// findings.md #0) — logSdkMessage now relays every SDK message type
// (system/assistant/user/result) plus PushToolTelemetry's tool_call, none
// of which are safe to post verbatim to Discord (raw Bash stdout/stderr,
// file contents, tool_use input). A denylist of exactly one type
// (tool_call) meant any *new* type nobody remembered to exclude would
// spam every task thread by default; an allowlist fails closed instead.
var discordSafeTypes = []string{"", "discussion", "approve", "abort", "question", "answer"}

func relayPending(ctx context.Context, pool *pgxpool.Pool, notifier Notifier) {
	// Everything not on the allowlist is flipped to relayed without ever
	// posting, before the SELECT below even sees it — cheap (self-clears
	// every tick), no schema change to the relay-tracking columns, and the
	// main query stays untouched. The transcript row itself is untouched
	// either way — only relayed_to_discord changes — so the dashboard
	// (which renders the full stream regardless) is unaffected.
	if _, err := pool.Exec(ctx, `
		UPDATE transcript SET relayed_to_discord = true
		WHERE relayed_to_discord = false AND COALESCE(type, '') != ALL($1)
	`, discordSafeTypes); err != nil {
		slog.Error("relay: skip non-Discord-safe entries", "error", err)
	}

	rows, err := pool.Query(ctx, `
		SELECT task_id, seq, "from", text, COALESCE(type, '')
		FROM transcript
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
				UPDATE transcript
				SET relay_attempts = relay_attempts + 1,
				    relay_last_error = $3,
				    relay_dead_letter = (relay_attempts + 1 >= $4)
				WHERE task_id = $1 AND seq = $2
			`, p.taskID, p.e.Seq, err.Error(), maxRelayAttempts)
			continue
		}
		_, _ = pool.Exec(ctx, `
			UPDATE transcript SET relayed_to_discord = true WHERE task_id = $1 AND seq = $2
		`, p.taskID, p.e.Seq)
	}
}
