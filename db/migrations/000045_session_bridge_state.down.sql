DO $$
BEGIN
  FOR i IN 1..20 LOOP
    BEGIN
      SET LOCAL lock_timeout = '3s';
      ALTER TABLE sessions DROP COLUMN bridge_parked_at, DROP COLUMN bridge_turn_seq, DROP COLUMN bridge_state;
      RETURN;
    EXCEPTION WHEN lock_not_available OR deadlock_detected THEN
      IF i = 20 THEN RAISE; END IF;
      PERFORM pg_sleep(0.5);
    END;
  END LOOP;
END $$;
