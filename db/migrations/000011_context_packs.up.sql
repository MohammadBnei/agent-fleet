-- docs/adr/0063. Live context packs: rules text and skills edited from the
-- console and rendered into every warm. Reviewed packs (packs/ in git) and
-- the per-warm context log arrive with the provisioner and worker steps.
CREATE TABLE packs (
  name        TEXT        PRIMARY KEY CHECK (name ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
  owner_group TEXT        NOT NULL DEFAULT 'platform-admins',
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Versions are immutable; the latest one is what a warm renders. No has_mcp:
-- a live pack is text and skills only, and only live packs can be attached.
CREATE TABLE pack_versions (
  pack       TEXT        NOT NULL REFERENCES packs (name) ON DELETE CASCADE,
  version    INT         NOT NULL,
  text       TEXT        NOT NULL DEFAULT '',
  skills     JSONB       NOT NULL DEFAULT '[]', -- [{name, description, body}], frontmatter rendered by core
  created_by TEXT        NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (pack, version)
);

-- target is a definition name, or a repo's GitHub id as text (ADR-0074): an
-- override rename leaves it alone, and a repo swept from the fleet keeps its
-- attachments for when the same GitHub repo rejoins. No FK on purpose.
-- Only live packs can be attached (the FK), so no attached pack carries MCP.
CREATE TABLE pack_attachments (
  target_kind TEXT        NOT NULL CHECK (target_kind IN ('definition', 'repo')),
  target      TEXT        NOT NULL,
  pack        TEXT        NOT NULL REFERENCES packs (name) ON DELETE CASCADE,
  attached_by TEXT        NOT NULL,
  attached_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (target_kind, target, pack)
);
