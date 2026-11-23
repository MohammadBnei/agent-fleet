UPDATE transcript SET type = NULL WHERE type = 'model_switch';
ALTER TABLE transcript DROP CONSTRAINT transcript_type_check;
ALTER TABLE transcript ADD CONSTRAINT transcript_type_check CHECK (type IN (
  'discussion', 'abort', 'interrupt', 'question', 'answer',
  'tool_call', 'system', 'assistant', 'user', 'result',
  'permission_mode', 'permission_request', 'permission_response', 'card'
) OR type IS NULL);
