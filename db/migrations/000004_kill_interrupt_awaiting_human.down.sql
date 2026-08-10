ALTER TABLE tasks DROP COLUMN awaiting_human;

ALTER TABLE transcript DROP CONSTRAINT transcript_type_check;
ALTER TABLE transcript ADD CONSTRAINT transcript_type_check CHECK (type IN (
  'discussion', 'approve', 'abort', 'question', 'answer',
  'tool_call', 'system', 'assistant', 'user', 'result', 'permission_mode',
  'permission_request', 'permission_response'
) OR type IS NULL);
