-- Session events to the event bridge (core/internal/sessionevents): what the bridge accepted, per session.
-- bridge_state: NULL nothing yet, 'opened', 'ended'. bridge_turn_seq: the highest result seq published, -1 none.
-- bridge_parked_at: set when the bridge refused an event retrying cannot fix; core tries again after 6 hours.
-- Columns only, all defaults constant, so no table rewrite. The ACCESS EXCLUSIVE lock on sessions waits at most
-- 3s per try; a passing reader or row lock makes it try again (20 tries, 0.5s apart) instead of failing the file.
-- If it still fails the version is dirty. Recovery: migrate force 43, then run the migration again
-- (the columns are not added when the file fails: the ALTER is one transaction).
DO $$
BEGIN
  FOR i IN 1..20 LOOP
    BEGIN
      SET LOCAL lock_timeout = '3s';
      ALTER TABLE sessions
        ADD COLUMN bridge_state TEXT,
        ADD COLUMN bridge_turn_seq BIGINT NOT NULL DEFAULT -1,
        ADD COLUMN bridge_parked_at TIMESTAMPTZ;
      RETURN;
    EXCEPTION WHEN lock_not_available OR deadlock_detected THEN
      IF i = 20 THEN RAISE; END IF;
      PERFORM pg_sleep(0.5);
    END;
  END LOOP;
END $$;
