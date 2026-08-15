-- Reverses 000001_init.up.sql completely.
--
-- This is a real down migration, not a stub, and that matters more than it
-- looks: migrations run as common-app-chart's hooks.migrate PreSync job, so
-- a failed apply leaves golang-migrate's schema_migrations row marked dirty,
-- and a dirty database blocks every subsequent core deploy until someone
-- fixes it by hand against production. A down that cannot run is a down
-- that turns one bad migration into an outage.
--
-- Order matters: transcript and proposals both reference sessions, so they
-- drop first. DROP TABLE removes the table's own indexes with it, so the
-- indexes created in the up migration need no separate statements here.

DROP TABLE IF EXISTS transcript;
DROP TABLE IF EXISTS proposals;
DROP TABLE IF EXISTS sessions;

DROP TABLE IF EXISTS knowledge_journal;
DROP TABLE IF EXISTS scheduled_audits;
DROP TABLE IF EXISTS prompt_snippets;
DROP TABLE IF EXISTS repos;
