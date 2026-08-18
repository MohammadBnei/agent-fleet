-- scheduled_audits was never dropped by the up migration, and its rows were
-- copied rather than moved, so rolling back restores a working old core
-- without touching it. What is lost is anything created or edited in
-- `schedules` after the up ran: cron and one-shot schedules have no
-- representation in the old table, and interval ones would come back with the
-- pre-migration cursor. Copying interval rows back would therefore resurrect
-- stale next_run_at values and silently drop the rest, which is worse than
-- losing the lot visibly.
DROP TABLE IF EXISTS schedules;

-- The narrowed CHECK cannot be added while a row still says 'schedule', and
-- those rows are real proposals a human may still be looking at — so relabel
-- rather than delete. They came from a schedule, which is what an audit was.
UPDATE proposals SET source = 'audit' WHERE source = 'schedule';

ALTER TABLE proposals DROP CONSTRAINT proposals_source_check;
ALTER TABLE proposals ADD  CONSTRAINT proposals_source_check
  CHECK (source IN ('alert', 'audit'));
