-- docs/adr/0084 §6, slice 8: a character's spawned run ends with a turn-starting entry in its transcript. Core's sweep
-- stamps the run once the entry is appended, so it is told once.
ALTER TABLE sessions ADD COLUMN spawn_reported_at TIMESTAMPTZ;
CREATE INDEX sessions_spawn_unreported_idx ON sessions (created_at)
  WHERE spawn_reported_at IS NULL AND lifecycle = 'single-shot' AND invoker LIKE 'standing:%';
