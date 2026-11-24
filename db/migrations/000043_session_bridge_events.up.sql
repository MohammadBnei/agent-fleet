-- Session events to the event bridge (core/internal/sessionevents): opened, turn_ended, ended.
-- Core keeps, per session, what the bridge accepted: bridge_state (NULL: nothing yet; opened;
-- ended) and bridge_turn_seq (the highest result entry published, in seq order). A refusal that
-- retrying cannot fix sets bridge_parked: the session leaves the pick and is counted.
-- Sessions and entries that exist now are marked published, so deploy sends no backlog.
ALTER TABLE sessions
  ADD COLUMN bridge_state TEXT CHECK (bridge_state IN ('opened', 'ended')),
  ADD COLUMN bridge_turn_seq BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN bridge_parked BOOLEAN NOT NULL DEFAULT false;
UPDATE sessions s SET
  bridge_state = CASE WHEN s.archived_at IS NULL THEN 'opened' ELSE 'ended' END,
  bridge_turn_seq = COALESCE((SELECT max(t.seq) FROM transcript t WHERE t.session_id = s.id), 0);
-- Finds a session's unpublished results without walking its whole transcript.
CREATE INDEX transcript_result_idx ON transcript (session_id, seq) WHERE type = 'result';
