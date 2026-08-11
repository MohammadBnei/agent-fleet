-- thot's activity now lives in `transcript` like every other session's
-- (docs/adr/0037), so this table has no readers left.
--
-- IRREVERSIBLE: this table holds real rows written during thot's first
-- day in production. The down migration recreates the empty table so a
-- rollback still has a valid schema, but the data is gone for good — it
-- was never a system of record for anything, only thot's own scratch
-- feed, which is why dropping it is acceptable rather than migrating it
-- into `transcript` (there is no task to attach those rows to).
DROP INDEX IF EXISTS idx_thot_events_reply_to;
DROP INDEX IF EXISTS idx_thot_events_id;
DROP TABLE IF EXISTS thot_events;
