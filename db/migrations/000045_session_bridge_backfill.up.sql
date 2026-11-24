-- Sessions and results that exist now count as published, so deploy sends no backlog (an open session
-- gets no opened event; its later turns and its ended do flow). One UPDATE of non-key columns takes row
-- locks that appends' FK check (FOR KEY SHARE) does not conflict with, and no table lock. Only rows the
-- new columns have not reached are touched, so a rerun is harmless.
SET lock_timeout = '3s';
UPDATE sessions s SET
  bridge_state = CASE WHEN s.archived_at IS NULL THEN 'opened' ELSE 'ended' END,
  bridge_turn_seq = COALESCE((SELECT max(t.seq) FROM transcript t WHERE t.session_id = s.id AND t.type = 'result'), -1)
WHERE s.bridge_state IS NULL;
RESET lock_timeout;
