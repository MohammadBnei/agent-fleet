-- Dashboard-editable periodic audits thot runs against the cluster
-- (docs/adr/0035). Same "edit it in the dashboard, no redeploy" shape the
-- `repos` table established (docs/adr/0028), and the same flat-relational
-- convention this schema uses everywhere: plain columns + CHECK
-- constraints, not JSONB, for anything with a bounded shape.
CREATE TABLE scheduled_audits (
  id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),

  -- Human-facing label, and the free-text instruction handed to thot.
  -- `prompt` is genuinely free-form: it's a question for an agent, not a
  -- structured query, so there's nothing to constrain it to.
  name             TEXT NOT NULL UNIQUE,
  prompt           TEXT NOT NULL,

  -- Interval seconds rather than a cron expression: no cron parser exists
  -- anywhere in this codebase, and every audit anyone has actually asked
  -- for is "every N hours". A cron column can be added later without
  -- touching this one if a real "every Monday 09:00" need shows up.
  interval_seconds INT NOT NULL CHECK (interval_seconds >= 60),

  enabled          BOOLEAN NOT NULL DEFAULT true,

  -- The scheduler's own cursor. next_run_at is what the loop actually
  -- queries on; last_run_at is for the UI.
  next_run_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_run_at      TIMESTAMPTZ,
  last_status      TEXT,

  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The audit loop's only hot query: due-and-enabled, in due order.
CREATE INDEX idx_scheduled_audits_due ON scheduled_audits (next_run_at) WHERE enabled;
