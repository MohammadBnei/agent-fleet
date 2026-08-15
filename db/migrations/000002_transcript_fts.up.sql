-- Full-text search over the session list (dashboard). ListSessions gains a
-- `query` param that matches session labels (repo/title/description) and any
-- transcript row's text. The session fields are tiny — no index there — but
-- the transcript is the fleet's largest table, so its FTS subquery gets a GIN
-- index over the same to_tsvector expression the query uses.
CREATE INDEX transcript_fts_idx ON transcript
  USING GIN (to_tsvector('english', text));
