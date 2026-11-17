DROP INDEX sessions_spawn_unreported_idx;
ALTER TABLE sessions DROP COLUMN spawn_reported_at;
