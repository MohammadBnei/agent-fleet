-- A standing row would fail the old CHECK: it becomes an archived resident thread, its transcript kept.
ALTER TABLE sessions
  DROP CONSTRAINT sessions_standing_only_fields,
  DROP CONSTRAINT sessions_standing_principal;
UPDATE sessions SET lifecycle = 'resident', archived_at = COALESCE(archived_at, now()) WHERE lifecycle = 'standing';
ALTER TABLE sessions
  DROP COLUMN stopped_at,
  DROP COLUMN started_by,
  DROP CONSTRAINT sessions_lifecycle_check,
  ADD CONSTRAINT sessions_lifecycle_check CHECK (lifecycle IN ('resident', 'single-shot'));
