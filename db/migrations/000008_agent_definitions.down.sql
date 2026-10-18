-- docs/adr/0062: archive definition sessions BEFORE rolling core back below
-- this migration. Removing the pin makes older code warm them as ordinary
-- repo sessions (GH_TOKEN, any permission mode).
ALTER TABLE sessions
  DROP CONSTRAINT sessions_definition_fk,
  DROP CONSTRAINT sessions_definition_pair,
  DROP COLUMN definition_sha,
  DROP COLUMN definition_name;
DROP TABLE agent_definitions;
