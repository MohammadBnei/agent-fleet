-- docs/adr/0082 §5-6: the decision card, the fourth kind. A card is a 'card'
-- transcript entry only core writes (decisions.Store.Draft), the protojson of
-- an agentfleetv1.Card; the append hook makes its row as it does a question's.
ALTER TABLE transcript DROP CONSTRAINT transcript_type_check;
ALTER TABLE transcript ADD CONSTRAINT transcript_type_check CHECK (type IN (
  'discussion', 'abort', 'interrupt', 'question', 'answer',
  'tool_call', 'system', 'assistant', 'user', 'result',
  'permission_mode', 'permission_request', 'permission_response', 'card'
) OR type IS NULL);

ALTER TABLE decisions DROP CONSTRAINT decisions_kind_check;
ALTER TABLE decisions ADD CONSTRAINT decisions_kind_check CHECK (kind IN ('question', 'permission', 'plan', 'card'));

-- A card's: its subject in thread-key form, github/<owner>/<name>/<kind>/<n>
-- or github/<owner>/<name> for a repo, and when it turns expired. NULL for
-- every other kind.
ALTER TABLE decisions
  ADD COLUMN subject    TEXT,
  ADD COLUMN expires_at TIMESTAMPTZ;

-- One open card per subject: a new draft on it makes the open one stale
-- first (decisions.OnAppend, under one advisory lock). lower(): a GitHub
-- owner or name differs only in case.
CREATE UNIQUE INDEX decisions_one_open_card_idx ON decisions (lower(subject))
  WHERE kind = 'card' AND state = 'open';
