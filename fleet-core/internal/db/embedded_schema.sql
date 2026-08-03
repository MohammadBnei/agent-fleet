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
  skip_critique     BOOLEAN NOT NULL DEFAULT false,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS tasks_repo_status_idx ON tasks (repo, status);

-- CREATE TABLE IF NOT EXISTS above is a no-op against the already-live
-- table, so new columns need their own idempotent statement here too.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS skip_critique BOOLEAN NOT NULL DEFAULT false;

-- Named + re-applied via DROP/ADD (not inline on the column) so adding a new
-- status later — like 'cancelled' below, for the round-cap/kill-switch
-- guardrails — is a safe re-run against the already-live table, not just
-- future fresh creates.
ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_status_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_status_check
  CHECK (status IN ('pending', 'claimed', 'planning', 'done', 'failed', 'cancelled'));

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
-- store for the proposer/critic/human planning conversation (see
-- docs/adr/0013). `seq` is the per-task monotonic cursor that replicates
-- LRANGE-from-index semantics — fleet-core computes it inside the same
-- transaction as the insert, guarded by
-- pg_advisory_xact_lock(hashtext(task_id::text)) so two near-simultaneous
-- appends (proposer + critic posting at once) can't race the same seq.
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

ALTER TABLE planning_transcript DROP CONSTRAINT IF EXISTS planning_transcript_type_check;
ALTER TABLE planning_transcript ADD CONSTRAINT planning_transcript_type_check
  CHECK (type IN ('discussion', 'approve', 'abort') OR type IS NULL);

-- Retry/DLQ for the Discord-relay side effect only — the transcript entry
-- itself is already durable the moment the row above commits; these track
-- whether fleet-core has successfully posted it to the Discord thread yet,
-- so a transient Discord API failure retries instead of crashing the whole
-- watch loop (today's unguarded postReply can do exactly that).
ALTER TABLE planning_transcript ADD COLUMN IF NOT EXISTS relayed_to_discord BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE planning_transcript ADD COLUMN IF NOT EXISTS relay_attempts INT NOT NULL DEFAULT 0;
ALTER TABLE planning_transcript ADD COLUMN IF NOT EXISTS relay_dead_letter BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE planning_transcript ADD COLUMN IF NOT EXISTS relay_last_error TEXT;
