-- docs/adr/0086 §5: a note written by a session whose set holds A is quarantined.
-- journal_search hides these rows until the owner promotes them.
ALTER TABLE knowledge_journal ADD COLUMN quarantined BOOLEAN NOT NULL DEFAULT false;
