-- agent-fleet shared task queue + knowledge journal.
-- Applied via common-app-chart's hooks.migrate PreSync job on the bot's Application.
-- Safe to re-run: every statement is idempotent.

CREATE TABLE IF NOT EXISTS tasks (
  id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  repo              TEXT NOT NULL,           -- 'dream-analyst' | 'vos-monolith'
  description       TEXT NOT NULL,
  status            TEXT NOT NULL DEFAULT 'pending',
  discord_channel_id TEXT NOT NULL,
  discord_thread_id  TEXT,
  claimed_by        TEXT,
  pr_url            TEXT,
  notes             TEXT,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS tasks_repo_status_idx ON tasks (repo, status);

-- skip_critique was the /task-time opt-out for the old proposer/critic
-- design (docs/adr/0011, superseded) — dead now that planning is a single
-- session and interview/doubt gating is the planner's own judgment call
-- (docs/adr/0017).
ALTER TABLE tasks DROP COLUMN IF EXISTS skip_critique;

-- Named + re-applied via DROP/ADD (not inline on the column) so adding a new
-- status later — like 'cancelled' below, for the round-cap/kill-switch
-- guardrails, and 'failed_permanently' (reliability-findings.md #1: a task
-- reclaimed past MAX_TASK_RETRIES stops retrying instead of looping
-- forever) — is a safe re-run against the already-live table, not just
-- future fresh creates.
ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_status_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_status_check
  CHECK (status IN ('pending', 'claimed', 'planning', 'implementing', 'done', 'failed', 'cancelled', 'failed_permanently'));

-- Crash recovery + resume (see docs/adr/0016). planning_session_id is the
-- Postgres-durable *pointer* to the single planner session's Claude session
-- (docs/adr/0017 — one session, not a proposer_session_id/critic_session_id
-- pair) whose actual transcript now lives on the worker's RWX PVC
-- (CLAUDE_CONFIG_DIR, see ADR-0016) — the id alone is useless without that
-- PVC-side change. heartbeat_at drives stale-claim reclaim in
-- claimNextTask; lease_id guards the rare split-brain case where a reclaim
-- raced a not-actually-dead worker.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS planning_session_id TEXT;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS retry_count INT NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS last_error TEXT;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS heartbeat_at TIMESTAMPTZ;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS lease_id UUID;

-- Which model actually ran this task's session — set alongside
-- planning_session_id (core/internal/tasks/store.go's SaveSessionID,
-- called by the sidecar on the worker's behalf, docs/adr/0020/0021). Only
-- new column the shared-PVC/provisioner/sidecar redesign (docs/adr/0019-
-- 0021) actually needed — confirmed by re-checking every query the
-- rewritten Go services issue against this table.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS model TEXT;

-- Tasks created from the dashboard (DashboardService.CreateTask) have no
-- Discord channel/thread at all — core/internal/discord/session.go's
-- PostToThread already no-ops when discord_thread_id is NULL, so relaxing
-- this is enough to support that origin without any other schema change.
ALTER TABLE tasks ALTER COLUMN discord_channel_id DROP NOT NULL;

-- DashboardService.DeleteTask soft-deletes: a hard DELETE would violate
-- planning_transcript/e2e_sessions' REFERENCES tasks(id) (no cascade) the
-- moment a task has any transcript history, which is effectively always.
-- GetTask/ListRecentTasks both filter WHERE deleted_at IS NULL.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;

-- Append-only fleet knowledge journal (mirrors ai-devkit's JSON-event pattern,
-- see agent-fleet reference-check memory: avoids write-conflict issues that a
-- shared mutable doc would hit across concurrent worker pods).
CREATE TABLE IF NOT EXISTS knowledge_journal (
  id         BIGSERIAL PRIMARY KEY,
  repo       TEXT,
  actor      TEXT NOT NULL,        -- 'dream-analyst-worker' | 'vos-monolith-worker' | 'bot'
  event_type TEXT NOT NULL,
  payload    JSONB NOT NULL DEFAULT '{}',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- On-demand e2e test environments: one row per requested/live/torn-down e2e
-- pod for a task. The single coordination point between the worker's tool
-- calls, the e2e-provisioner's reconcile loop, and the bot's /e2e-kill
-- command — none of them talk to each other directly, only through this
-- table (+ tasks.status for the terminal-state teardown trigger).
CREATE TABLE IF NOT EXISTS e2e_sessions (
  id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  task_id        UUID NOT NULL REFERENCES tasks(id),
  status         TEXT NOT NULL DEFAULT 'requested',
  pod_name       TEXT,
  ingress_path   TEXT,
  kill_requested BOOLEAN NOT NULL DEFAULT false,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS e2e_sessions_task_idx ON e2e_sessions (task_id);

ALTER TABLE e2e_sessions DROP CONSTRAINT IF EXISTS e2e_sessions_status_check;
ALTER TABLE e2e_sessions ADD CONSTRAINT e2e_sessions_status_check
  CHECK (status IN ('requested', 'running', 'failed', 'torn_down'));

-- Observability only (which trigger caused a given teardown: human
-- /e2e-kill vs agent kill_env vs terminal-status auto-teardown) — not a
-- dedup mechanism, kill_requested=true is already idempotent to set twice.
ALTER TABLE e2e_sessions ADD COLUMN IF NOT EXISTS kill_idempotency_key TEXT;

-- Replaces the Redis list (agentfleet:planning:<taskId>) as the durable
-- store for the planner/human planning conversation (see docs/adr/0013;
-- "planner" not "proposer/critic" as of docs/adr/0017). `seq` is the
-- per-task monotonic cursor that replicates LRANGE-from-index semantics —
-- fleet-core computes it inside the same transaction as the insert,
-- guarded by pg_advisory_xact_lock(hashtext(task_id::text)) so a human's
-- Discord reply and the planner's own concurrent send_message call can't
-- race the same seq.
CREATE TABLE IF NOT EXISTS planning_transcript (
  task_id         UUID NOT NULL REFERENCES tasks(id),
  seq             BIGINT NOT NULL,
  "from"          TEXT NOT NULL,
  text            TEXT NOT NULL,
  type            TEXT,
  idempotency_key TEXT NOT NULL,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (task_id, seq)
);

CREATE INDEX IF NOT EXISTS planning_transcript_task_seq_idx
  ON planning_transcript (task_id, seq);

-- Enforces the actual idempotency guarantee: retrying an append with the
-- same (task_id, idempotency_key) must not double-post a message.
CREATE UNIQUE INDEX IF NOT EXISTS planning_transcript_idempotency_idx
  ON planning_transcript (task_id, idempotency_key);

-- 'question'/'answer' carry a JSON payload in `text`, not prose — the
-- AskUserQuestion MCP tool (fleet-core/internal/mcpserver) posts a
-- 'question' entry and long-polls for the matching 'answer' entry, which
-- the dashboard (not Discord) submits via AnswerQuestion. See docs/adr/0018.
--
-- 'system'/'assistant'/'user'/'result' are the SDK's own raw message
-- discriminants (reliability-findings.md #0's "relay everything, let the
-- UI decide" — worker/src/planning.ts's logSdkMessage tags every message
-- with its own msg.type verbatim, not just assistant text as before).
-- 'tool_call' is PushToolTelemetry's sidecar-pushed summary — already
-- inserted before this change (coreserver/server.go), just never actually
-- listed here; a pre-existing gap this widening also closes. This embedded
-- copy had drifted from db/schema.sql (still missing 'tool_call' etc. here
-- despite the canonical file already having them) — CI's drift diff should
-- have caught this but evidently didn't; found while seeding local test
-- data for the mobile dashboard work and inserting a 'tool_call' row
-- failed the CHECK constraint against this file's copy.
ALTER TABLE planning_transcript DROP CONSTRAINT IF EXISTS planning_transcript_type_check;
ALTER TABLE planning_transcript ADD CONSTRAINT planning_transcript_type_check
  CHECK (type IN (
    'discussion', 'approve', 'abort', 'question', 'answer',
    'tool_call', 'system', 'assistant', 'user', 'result'
  ) OR type IS NULL);

-- Retry/DLQ for the Discord-relay side effect only — the transcript entry
-- itself is already durable the moment the row above commits; these track
-- whether fleet-core has successfully posted it to the Discord thread yet,
-- so a transient Discord API failure retries instead of crashing the whole
-- watch loop (today's unguarded postReply can do exactly that).
ALTER TABLE planning_transcript ADD COLUMN IF NOT EXISTS relayed_to_discord BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE planning_transcript ADD COLUMN IF NOT EXISTS relay_attempts INT NOT NULL DEFAULT 0;
ALTER TABLE planning_transcript ADD COLUMN IF NOT EXISTS relay_dead_letter BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE planning_transcript ADD COLUMN IF NOT EXISTS relay_last_error TEXT;

-- reply_to_seq is the question-seq correlation reliability-findings.md #0
-- calls out as a real gap: today's "any pending question + any reply"
-- would let an unrelated message satisfy a blocked AskUserQuestion call.
-- NULL for every entry except an 'answer' replying to a specific
-- 'question' entry's own seq. This embedded copy was missing the column
-- entirely (same drift as the CHECK constraint above) — transcript.
-- PostgresStore.AppendReply (the AnswerQuestion RPC's write path) writes
-- to it unconditionally, so every real deployment applying only this file
-- would have hard-failed on the first answered question.
ALTER TABLE planning_transcript ADD COLUMN IF NOT EXISTS reply_to_seq BIGINT;
