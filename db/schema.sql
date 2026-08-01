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
