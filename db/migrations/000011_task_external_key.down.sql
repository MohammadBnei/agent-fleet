DROP INDEX IF EXISTS idx_tasks_external_key_active;
ALTER TABLE tasks DROP COLUMN IF EXISTS external_key;
