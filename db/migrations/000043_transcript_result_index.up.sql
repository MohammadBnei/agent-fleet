-- Finds a session's unpublished results without walking its whole transcript (core/internal/sessionevents).
-- One statement alone in this file: golang-migrate runs a file as a single simple query, so a lone
-- CREATE INDEX CONCURRENTLY runs outside a transaction block (it would fail beside a second statement).
-- It takes no lock that blocks appends. If it fails it leaves an INVALID index: drop it, then migrate again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS transcript_result_idx ON transcript (session_id, seq) WHERE type = 'result';
