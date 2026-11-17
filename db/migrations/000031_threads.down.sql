DROP TABLE session_threads;
DROP INDEX transcript_thread_idx;
ALTER TABLE transcript DROP COLUMN thread_key;
