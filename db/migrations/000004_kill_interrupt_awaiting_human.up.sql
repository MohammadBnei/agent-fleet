-- Widen transcript.type to allow "interrupt" (DashboardService.Interrupt —
-- stops only the current turn, unlike "abort" which ends the whole
-- session/pod). Constraint name confirmed against a real Postgres instance
-- (auto-generated from the unmodified column-level CHECK in 000001_init).
ALTER TABLE transcript DROP CONSTRAINT transcript_type_check;
ALTER TABLE transcript ADD CONSTRAINT transcript_type_check CHECK (type IN (
  'discussion', 'approve', 'abort', 'question', 'answer',
  'tool_call', 'system', 'assistant', 'user', 'result', 'permission_mode',
  'permission_request', 'permission_response', 'interrupt'
) OR type IS NULL);

-- True while an unresolved permission_request/question transcript entry is
-- outstanding — set/cleared by core's activityTrackingStore decorator, the
-- same choke point that already maintains last_active_at. Lets the
-- dashboard's task list show which tasks need a human decision without an
-- N+1 per-task transcript fetch.
ALTER TABLE tasks ADD COLUMN awaiting_human BOOLEAN NOT NULL DEFAULT false;
