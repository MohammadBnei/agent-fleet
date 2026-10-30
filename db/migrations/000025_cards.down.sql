-- The card entries stay, as untyped transcript text; their decisions go,
-- since the kind no longer exists.
DROP INDEX decisions_one_open_card_idx;
DELETE FROM decisions WHERE kind = 'card';
ALTER TABLE decisions DROP COLUMN subject, DROP COLUMN expires_at;
ALTER TABLE decisions DROP CONSTRAINT decisions_kind_check;
ALTER TABLE decisions ADD CONSTRAINT decisions_kind_check CHECK (kind IN ('question', 'permission', 'plan'));

UPDATE transcript SET type = NULL WHERE type = 'card';
ALTER TABLE transcript DROP CONSTRAINT transcript_type_check;
ALTER TABLE transcript ADD CONSTRAINT transcript_type_check CHECK (type IN (
  'discussion', 'abort', 'interrupt', 'question', 'answer',
  'tool_call', 'system', 'assistant', 'user', 'result',
  'permission_mode', 'permission_request', 'permission_response'
) OR type IS NULL);
