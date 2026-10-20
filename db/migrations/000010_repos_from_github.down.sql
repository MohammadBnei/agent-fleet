-- docs/adr/0074. Gone rows come back as visible: the down path has no sync
-- to hide them, so they are removed rather than resurrected.
DELETE FROM repos WHERE gone_since IS NOT NULL;
ALTER TABLE repos
  DROP COLUMN gone_since,
  DROP COLUMN full_name,
  DROP COLUMN github_id;
