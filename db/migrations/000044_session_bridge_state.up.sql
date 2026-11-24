-- Session events to the event bridge (core/internal/sessionevents): what the bridge accepted, per session.
-- bridge_state: NULL nothing yet, 'opened', 'ended'. bridge_turn_seq: the highest result seq published, -1 none.
-- bridge_parked_at: set when the bridge refused an event retrying cannot fix; core tries again after 6 hours.
-- Columns only, all defaults constant, so no table rewrite. The brief ACCESS EXCLUSIVE lock on sessions
-- gives up after 3s instead of queueing behind (and in front of) appends; migrate again then.
SET lock_timeout = '3s';
ALTER TABLE sessions
  ADD COLUMN bridge_state TEXT,
  ADD COLUMN bridge_turn_seq BIGINT NOT NULL DEFAULT -1,
  ADD COLUMN bridge_parked_at TIMESTAMPTZ;
RESET lock_timeout;
