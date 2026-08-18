-- ---------------------------------------------------------------------------
-- schedules
-- ---------------------------------------------------------------------------
-- Generalizes scheduled_audits (docs/adr/0035) from "periodic cluster checks
-- against infra-bootstrap" to "scheduled work against any repo". Three things
-- change: the repo is data instead of a Go constant (it was
-- `const auditRepo = "infra-bootstrap"`), the cadence can be a cron
-- expression, and a schedule can be one-shot.
--
-- scheduled_audits is deliberately NOT dropped here. Migrations run as
-- common-app-chart's hooks.migrate PreSync job, so this executes BEFORE the
-- new core rolls out — dropping the table in the same release would have the
-- still-running old core error every 60s in ClaimDue and 500 on
-- ListScheduledAudits for the length of the rollout. It also keeps this
-- release's rollback lossless: the old core finds its table intact. The drop
-- is 000006, in a later release.
CREATE TABLE schedules (
  id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),

  -- Unique per repo, not globally: two repos both wanting a `weekly-rundown`
  -- is the normal case once the repo is no longer a constant.
  name             TEXT NOT NULL,
  repo             TEXT NOT NULL,
  prompt           TEXT NOT NULL,

  -- Exactly one timing model, or neither:
  --   cron IS NOT NULL             -> fires on the expression (robfig/cron
  --                                   ParseStandard, which accepts a
  --                                   `CRON_TZ=Europe/Paris ` prefix — that is
  --                                   why there is no timezone column)
  --   interval_seconds IS NOT NULL -> every N seconds, as before
  --   neither                      -> one-shot at next_run_at, then disabled
  --
  -- Both NULL is the one-shot case, so the CHECK forbids only "both set".
  -- Nothing here parses cron: an expression Postgres cannot evaluate is
  -- validated in Go at the trust boundary (Create/Update), where the human
  -- who typed it gets the error.
  cron             TEXT,
  interval_seconds INT CHECK (interval_seconds >= 60),

  -- "Run now" as a flag rather than `next_run_at = now()`. Moving the cursor
  -- eats a cron occurrence — run-now on a Sunday for `0 9 * * MON` would
  -- consume Monday 09:00 and the schedule would silently skip a week. A flag
  -- fires out of band and leaves the anchor alone.
  run_now          BOOLEAN NOT NULL DEFAULT false,

  enabled          BOOLEAN NOT NULL DEFAULT true,
  next_run_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_run_at      TIMESTAMPTZ,
  last_status      TEXT,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

  UNIQUE (repo, name),
  CHECK (cron IS NULL OR interval_seconds IS NULL)
);

-- run_now rows are not covered by this partial index (they may be disabled,
-- and their next_run_at is in the future). That is fine: it is a boolean-true
-- scan over a table with tens of rows, not thousands.
CREATE INDEX idx_schedules_due ON schedules (next_run_at) WHERE enabled;

-- Every existing audit keeps firing exactly as it did, against the repo the
-- constant named.
-- Ids are carried over, not regenerated: the proposals dedup key embeds the
-- schedule's id, and a new id means a standing proposal stops collapsing the
-- next tick into itself — duplicates at exactly the cutover.
INSERT INTO schedules (id, name, repo, prompt, interval_seconds, enabled, next_run_at, last_run_at, last_status)
  SELECT id, name, 'infra-bootstrap', prompt, interval_seconds, enabled, next_run_at, last_run_at, last_status
  FROM scheduled_audits;

-- And the standing proposals move with them, for the same reason: the key is
-- `audit:<id>` and the loop now writes `schedule:<id>`.
UPDATE proposals SET dedup_key = 'schedule:' || substring(dedup_key from 7)
  WHERE dedup_key LIKE 'audit:%' AND dismissed_at IS NULL;

-- proposals.source is a CHECK-constrained enum, so a schedule filing under a
-- new source name would violate it on every fire — and loop.tick swallows that
-- into last_status, which means the cadence stops with nothing reported
-- anywhere. Widen it. Rows written before this keep source='audit'.
ALTER TABLE proposals DROP CONSTRAINT proposals_source_check;
ALTER TABLE proposals ADD  CONSTRAINT proposals_source_check
  CHECK (source IN ('alert', 'audit', 'schedule'));
