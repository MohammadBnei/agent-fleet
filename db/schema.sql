-- agent-fleet shared task queue + knowledge journal.
-- Applied via common-app-chart's hooks.migrate PreSync job on the bot's Application.
-- Safe to re-run: every statement is idempotent.

CREATE TABLE IF NOT EXISTS tasks (
  id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  repo              TEXT NOT NULL,           -- 'dream-analyst' | 'vos-monolith'
  description       TEXT NOT NULL,
  status            TEXT NOT NULL DEFAULT 'pending'
                      CHECK (status IN ('pending', 'claimed', 'planning', 'done', 'failed')),
  discord_channel_id TEXT NOT NULL,
  discord_thread_id  TEXT,
  claimed_by        TEXT,
  pr_url            TEXT,
  notes             TEXT,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS tasks_repo_status_idx ON tasks (repo, status);

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
