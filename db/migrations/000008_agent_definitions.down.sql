-- docs/adr/0062: removing the pin would make older code warm definition
-- sessions as ordinary repo sessions (GH_TOKEN, any permission mode), so
-- archive them first: an archived session never warms again.
UPDATE sessions SET archived_at = now()
 WHERE definition_name IS NOT NULL AND archived_at IS NULL;
ALTER TABLE sessions
  DROP CONSTRAINT sessions_definition_fk,
  DROP CONSTRAINT sessions_definition_pair,
  DROP COLUMN definition_sha,
  DROP COLUMN definition_name;
DROP TABLE agent_definitions;
