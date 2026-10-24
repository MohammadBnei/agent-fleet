-- Recreates the table and its index exactly as 000001 did, empty. Its rows
-- live on in schedules (copied by 000005); nothing reads this table, so an
-- empty one is a faithful rollback of the schema, not of data no one uses.
CREATE TABLE IF NOT EXISTS scheduled_audits (
  id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  name             TEXT NOT NULL UNIQUE,
  prompt           TEXT NOT NULL,
  interval_seconds INT NOT NULL CHECK (interval_seconds >= 60),
  enabled          BOOLEAN NOT NULL DEFAULT true,
  next_run_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_run_at      TIMESTAMPTZ,
  last_status      TEXT,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_scheduled_audits_due ON scheduled_audits (next_run_at) WHERE enabled;
