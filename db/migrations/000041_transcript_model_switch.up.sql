-- The seam a model switch leaves in a session's feed (SetSessionModel): a 'model_switch' entry, {from, to}.
ALTER TABLE transcript DROP CONSTRAINT transcript_type_check;
ALTER TABLE transcript ADD CONSTRAINT transcript_type_check CHECK (type IN (
  'discussion', 'abort', 'interrupt', 'question', 'answer',
  'tool_call', 'system', 'assistant', 'user', 'result',
  'permission_mode', 'permission_request', 'permission_response', 'card', 'model_switch'
) OR type IS NULL);
