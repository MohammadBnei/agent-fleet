-- Full-text search over knowledge_journal (event_type + payload), backing
-- the journal_search MCP tool. Postgres to_tsvector/ts_rank — no new
-- dependency, no embedding model, no vector store (docs/adr/0032's
-- deferred "feed knowledge_journal back into a session" read path).
CREATE INDEX knowledge_journal_fts_idx ON knowledge_journal
  USING GIN (to_tsvector('english', event_type || ' ' || payload::text));
