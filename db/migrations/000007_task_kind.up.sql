-- What kind of work a task represents (docs/adr/0037).
--
-- 'worker' is every task the fleet has ever had: an agent working on a
-- target repo's feature. 'thot' is a cluster-agent session — still an
-- ordinary worker pod, still dispatched by the unmodified dispatch loop,
-- but pointed at infra-bootstrap with cluster access and labelled
-- differently in the UI.
--
-- Deliberately NOT a new SessionKind: that enum is a pod-teardown
-- discriminant consumed only by the provisioner, and thot needs no
-- special teardown. This column exists purely so the dashboard can label
-- and gate; the dispatch path never reads it.
ALTER TABLE tasks
  ADD COLUMN kind TEXT NOT NULL DEFAULT 'worker'
    CHECK (kind IN ('worker', 'thot'));

-- The dashboard filters the task list by kind on every poll.
CREATE INDEX idx_tasks_kind ON tasks (kind);
