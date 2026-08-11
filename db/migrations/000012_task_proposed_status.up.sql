-- 'proposed' is the human gate on machine-created thot tasks.
--
-- Alertmanager and the audit scheduler both create tasks with no human in
-- the loop, and a thot task runs an agent with cluster access. A proposal
-- is a task dispatch cannot see: tasks.Store.ClaimNextTask only claims
-- 'pending' (plus stale-heartbeat 'claimed'/'running'), so parking a row
-- here needs no dispatch change at all. A human releases it into the
-- normal queue via DashboardService.ApproveTask.
--
-- Constraint name confirmed against a real Postgres instance
-- (auto-generated from the unmodified column-level CHECK in 000001_init),
-- same as 000004. Deliberately not DROP ... IF EXISTS: if the name is ever
-- wrong, this must fail loudly at migrate time rather than silently leave
-- the old narrow constraint in place alongside a new one, which would
-- reject every proposal at runtime instead.
ALTER TABLE tasks DROP CONSTRAINT tasks_status_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_status_check CHECK (status IN (
  'proposed', 'pending', 'claimed', 'running',
  'done', 'failed', 'cancelled', 'failed_permanently'
));

-- No change to idx_tasks_external_key_active (000011). Its predicate is a
-- negative list -- status NOT IN ('done','failed','cancelled',
-- 'failed_permanently') -- so 'proposed' counts as active for free: an
-- un-approved proposal correctly blocks a re-firing alert from creating a
-- duplicate, and dismissing one (soft delete) frees the key so the alert
-- can be proposed again next time it fires.
