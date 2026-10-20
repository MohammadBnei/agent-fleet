-- docs/adr/0063. The last good copy of packs/ at the release tag, so a warm
-- never blocks on the definitions sync. One row per pack name; a pack whose
-- files fail to parse keeps its previous row.
CREATE TABLE reviewed_packs (
  name      TEXT        PRIMARY KEY,
  sha       TEXT        NOT NULL,
  text      TEXT        NOT NULL DEFAULT '',
  skills    JSONB       NOT NULL DEFAULT '[]', -- [{name, body}], body = SKILL.md as written
  error     TEXT,                              -- why the last sync kept the previous content
  synced_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
