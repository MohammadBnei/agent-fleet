-- The last state of a card the event bridge accepted (core/internal/cardevents,
-- docs/adr/0078 amendment): a card is published whenever its state differs
-- from this one. Current state, like session_announcements, not a queue
-- (docs/adr/0048 §2); a transition that happens between two ticks is not seen.
-- NULL: never published. Cards that exist now were not announced and are not
-- backfilled onto the bridge: they start out as published.
ALTER TABLE decisions ADD COLUMN bridge_state TEXT;
UPDATE decisions SET bridge_state = state WHERE kind = 'card';
