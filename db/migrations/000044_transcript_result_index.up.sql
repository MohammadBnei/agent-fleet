-- Finds a session's unpublished results without walking its whole transcript (core/internal/sessionevents).
-- One statement alone in this file: golang-migrate runs a file as a single simple query, so a lone
-- CREATE INDEX CONCURRENTLY runs outside a transaction block (it would fail beside a second statement).
-- It takes no lock that blocks appends. No IF NOT EXISTS: a leftover INVALID index must fail loudly.
-- If it fails the version is dirty (the migrate hook does not retry). Recovery, in this order:
--   DROP INDEX IF EXISTS transcript_result_idx;   then   migrate force 43   then run the migration again.
CREATE INDEX CONCURRENTLY transcript_result_idx ON transcript (session_id, seq) WHERE type = 'result';
