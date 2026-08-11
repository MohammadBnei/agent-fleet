-- Rows first: ADD CONSTRAINT below is validated against existing data, so
-- a single live proposal would make this migration fail halfway.
--
-- An un-approved proposal is exactly a task nobody chose to run, which is
-- what 'cancelled' means. It is also terminal, so this additionally frees
-- the alert's dedup key -- after a down-migration the same alert can be
-- proposed again on its next fire rather than being suppressed forever by
-- a row nothing can reach.
UPDATE tasks SET status = 'cancelled' WHERE status = 'proposed';

ALTER TABLE tasks DROP CONSTRAINT tasks_status_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_status_check CHECK (status IN (
  'pending', 'claimed', 'running', 'done', 'failed', 'cancelled', 'failed_permanently'
));
