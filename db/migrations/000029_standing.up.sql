-- docs/adr/0084 §2-§3, slice 3: a standing character is one session row, lifecycle 'standing', owned by its own
-- principal standing:<name> (so 000015's sessions_invoker_owns and sessions_thread_key_invoker pass as they are).
ALTER TABLE sessions DROP CONSTRAINT sessions_lifecycle_check;
ALTER TABLE sessions
  ADD CONSTRAINT sessions_lifecycle_check CHECK (lifecycle IN ('resident', 'single-shot', 'standing')),
  -- The admin who started it; the owner column is the principal, never a person.
  ADD COLUMN started_by TEXT,
  -- Stop marks the row. Start clears it. NULL on every other row.
  ADD COLUMN stopped_at TIMESTAMPTZ,
  ADD CONSTRAINT sessions_standing_principal CHECK (
    lifecycle IS DISTINCT FROM 'standing'
    OR (invoker = 'standing:' || definition_name AND thread_key = 'main' AND started_by IS NOT NULL)
  ),
  ADD CONSTRAINT sessions_standing_only_fields CHECK (
    lifecycle = 'standing' OR (started_by IS NULL AND stopped_at IS NULL)
  );
