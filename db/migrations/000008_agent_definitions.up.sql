-- docs/adr/0062. One row per (definition, git SHA where this content first
-- appeared). Rows are never deleted: sessions pin them.
CREATE TABLE agent_definitions (
  name       TEXT        NOT NULL,
  sha        TEXT        NOT NULL,
  spec       JSONB,                 -- the file's own spec; NULL when the YAML did not decode
  effective  JSONB,                 -- resolved spec; kept when a re-judge flips a row invalid
  valid      BOOLEAN     NOT NULL,
  error      TEXT,
  synced_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (name, sha),
  CONSTRAINT agent_definitions_valid_error CHECK (valid = (error IS NULL)),
  CONSTRAINT agent_definitions_valid_effective CHECK (NOT valid OR effective IS NOT NULL)
);

-- The pair CHECK is load-bearing: a composite FK is MATCH SIMPLE, so a
-- half-NULL pair would skip the FK entirely.
ALTER TABLE sessions
  ADD COLUMN definition_name TEXT,
  ADD COLUMN definition_sha  TEXT,
  ADD CONSTRAINT sessions_definition_pair CHECK ((definition_name IS NULL) = (definition_sha IS NULL)),
  ADD CONSTRAINT sessions_definition_fk FOREIGN KEY (definition_name, definition_sha)
      REFERENCES agent_definitions (name, sha),
  -- No definition session can hold a lease, so no core can warm one: not
  -- this one (it refuses first), and not an older one that knows nothing of
  -- definitions (a rollout overlap, an image rollback with the schema left
  -- here), which would warm it as a repo session (GH_TOKEN, any mode, no
  -- pool). docs/adr/0064's migration drops this with the code that sends the
  -- definition pod profile.
  ADD CONSTRAINT sessions_definition_no_lease CHECK (definition_name IS NULL OR lease_id IS NULL);
