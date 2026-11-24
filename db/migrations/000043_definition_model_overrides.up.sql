-- One console-chosen model per definition name (docs/adr/0088 amendment). The
-- definition's file in git stays the default and the bound; this row only
-- picks within it. Read at session create and standing restart, never by a
-- live session. No foreign key: a definition lives in git, and a row for a
-- name that has gone is harmless and ignored.
CREATE TABLE definition_model_overrides (
  definition_name TEXT PRIMARY KEY,
  model           TEXT NOT NULL CHECK (model <> ''),
  set_by          TEXT NOT NULL,
  set_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
