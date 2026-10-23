DROP TABLE dispatch_decisions;
DELETE FROM proposals WHERE source = 'event';
ALTER TABLE proposals
  DROP CONSTRAINT proposals_event_fields,
  DROP COLUMN tainted,
  DROP COLUMN causation_id,
  DROP COLUMN thread_key,
  DROP COLUMN definition_name;
ALTER TABLE proposals DROP CONSTRAINT proposals_source_check;
ALTER TABLE proposals ADD  CONSTRAINT proposals_source_check
  CHECK (source IN ('alert', 'audit', 'schedule'));
