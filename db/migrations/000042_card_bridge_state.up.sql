-- The last state of a card the event bridge accepted (core/internal/cardevents,
-- docs/adr/0078 amendment): a card is published whenever its state differs
-- from this one. Current state, like session_announcements, not a queue
-- (docs/adr/0048 §2); a transition that happens between two ticks is not seen.
-- NULL: never published. Closed cards that exist now are marked published; open
-- ones stay NULL, so the first tick after deploy publishes each as `opened`.
-- bridge_parked: the bridge refused the event (InvalidArgument, PermissionDenied);
-- it is out of the pick and counted by agentfleet_card_events_parked.
ALTER TABLE decisions ADD COLUMN bridge_state TEXT, ADD COLUMN bridge_parked BOOLEAN NOT NULL DEFAULT false;
UPDATE decisions SET bridge_state = state WHERE kind = 'card' AND state <> 'open';
CREATE INDEX decisions_card_unpublished_idx ON decisions (version)
  WHERE kind = 'card' AND NOT bridge_parked AND state IS DISTINCT FROM bridge_state;
