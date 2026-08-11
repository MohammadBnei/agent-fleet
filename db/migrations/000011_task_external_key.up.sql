-- Dedup key for tasks created by machinery rather than a human
-- (docs/adr/0037's alert path).
--
-- Alertmanager re-sends a firing alert every repeat_interval (4h today)
-- and groups can re-fire on any label change. Without dedup, one flapping
-- alert becomes an unbounded stream of tasks, each spawning a pod — which
-- would exhaust MAX_IN_FLIGHT_TASKS and starve real work. That is a much
-- worse outage than the alert itself.
ALTER TABLE tasks ADD COLUMN external_key TEXT;

-- One ACTIVE task per external key. Partial, so a resolved-then-refiring
-- alert legitimately opens a new task once the old one is finished — the
-- point is to avoid piling up duplicates while one is still being worked,
-- not to permanently bind an alert to its first task.
CREATE UNIQUE INDEX idx_tasks_external_key_active
  ON tasks (external_key)
  WHERE external_key IS NOT NULL
    AND deleted_at IS NULL
    AND status NOT IN ('done', 'failed', 'cancelled', 'failed_permanently');
