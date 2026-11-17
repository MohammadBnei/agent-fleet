-- docs/adr/0084 §4: a crash counts once per turn attempt. The highest agent seq of a thread seen at its last settled
-- crash: a CRASHED report with no agent output above it (a re-sighting after a core restart, a re-warm that failed
-- before dispatch) is the same attempt, never another crash.
ALTER TABLE session_threads ADD COLUMN crash_agent_seq BIGINT;
