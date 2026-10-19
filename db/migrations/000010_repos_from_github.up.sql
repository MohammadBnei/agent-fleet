-- docs/adr/0074. repos is projected from GitHub (the bot's collaborator list)
-- plus infra-bootstrap's overrides file. github_id is the identity: a rename
-- moves full_name and url on the same row, and an override pinned to an id
-- never follows a name someone else re-created. NULL only until the first
-- sync matches a pre-existing row by URL.
--
-- gone_since is the soft delete: set when a complete listing no longer has
-- the repo, which hides it and refuses new sessions; the row is deleted 7
-- days later once no open session references it, and cleared if the bot is
-- re-added first.
ALTER TABLE repos
  ADD COLUMN github_id  BIGINT UNIQUE,
  ADD COLUMN full_name  TEXT NOT NULL DEFAULT '',
  ADD COLUMN gone_since TIMESTAMPTZ;
