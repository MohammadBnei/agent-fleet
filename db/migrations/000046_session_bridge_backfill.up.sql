-- Sessions and results that exist now count as published, so deploy sends no backlog (an open session
-- gets no opened event; its later turns and its ended do flow). One UPDATE of non-key columns takes row
-- locks that appends' FK check (FOR KEY SHARE) does not conflict with, and no table lock. Only rows the
-- new columns have not reached are touched, so a rerun is harmless. A row lock held by someone else makes it
-- wait at most 3s per try and try again (20 tries, 0.5s apart) instead of failing the file.
-- If it still fails the version is dirty. Recovery: migrate force 45, then run the migration again.
DO $$
BEGIN
  FOR i IN 1..20 LOOP
    BEGIN
      SET LOCAL lock_timeout = '3s';
      UPDATE sessions s SET
        bridge_state = CASE WHEN s.archived_at IS NULL THEN 'opened' ELSE 'ended' END,
        bridge_turn_seq = COALESCE((SELECT max(t.seq) FROM transcript t WHERE t.session_id = s.id AND t.type = 'result'), -1)
      WHERE s.bridge_state IS NULL;
      RETURN;
    EXCEPTION WHEN lock_not_available OR deadlock_detected THEN
      IF i = 20 THEN RAISE; END IF;
      PERFORM pg_sleep(0.5);
    END;
  END LOOP;
END $$;
