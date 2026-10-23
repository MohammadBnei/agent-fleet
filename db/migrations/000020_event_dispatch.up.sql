-- docs/adr/0078 §5: what the bridge's dispatcher needs in core's schema.

-- An event proposal keeps what opening it needs: the definition and thread it
-- was routed to, the event that caused it, and its taint. Opening one opens
-- that definition's thread, not a plain repo session.
ALTER TABLE proposals DROP CONSTRAINT proposals_source_check;
ALTER TABLE proposals ADD  CONSTRAINT proposals_source_check
  CHECK (source IN ('alert', 'audit', 'schedule', 'event'));
ALTER TABLE proposals
  ADD COLUMN definition_name TEXT,
  ADD COLUMN thread_key      TEXT,
  ADD COLUMN causation_id    TEXT,
  ADD COLUMN tainted         BOOLEAN NOT NULL DEFAULT false,
  ADD CONSTRAINT proposals_event_fields CHECK (
    (source = 'event') = (definition_name IS NOT NULL AND thread_key IS NOT NULL));

-- The gate's decision per event and target definition, written before the
-- call and reused on redelivery: its inputs (the client's actors, the
-- thread's state) can change in between. outcome is NULL until the event was
-- handled, so a redelivery after a crash retries the same decision.
CREATE TABLE dispatch_decisions (
  source     TEXT        NOT NULL,
  event_id   TEXT        NOT NULL,
  definition TEXT        NOT NULL,
  wake       BOOLEAN     NOT NULL,
  tainted    BOOLEAN     NOT NULL,
  outcome    TEXT CHECK (outcome IN ('delivered', 'proposal', 'closed', 'skipped')),
  decided_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (source, event_id, definition)
);
-- The bridge deletes events after 30 days; the dispatcher prunes past that.
CREATE INDEX dispatch_decisions_decided_idx ON dispatch_decisions (decided_at);
