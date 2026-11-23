-- A thumb on one result entry (seq) or a 1..5 score on a whole session (seq NULL), each stamped with the model
-- that produced it. One thumb per (session, seq) and one score per session, both upserted.
CREATE TABLE session_ratings (
  id         BIGSERIAL PRIMARY KEY,
  session_id UUID NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
  seq        BIGINT,
  kind       TEXT NOT NULL CHECK (kind IN ('thumb', 'score')),
  value      INT NOT NULL,
  note       TEXT NOT NULL DEFAULT '',
  model      TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK ((kind = 'thumb' AND seq IS NOT NULL AND value IN (-1, 1))
      OR (kind = 'score' AND seq IS NULL AND value BETWEEN 1 AND 5))
);
CREATE UNIQUE INDEX session_ratings_thumb_idx ON session_ratings (session_id, seq) WHERE kind = 'thumb';
CREATE UNIQUE INDEX session_ratings_score_idx ON session_ratings (session_id) WHERE kind = 'score';
CREATE INDEX session_ratings_created_idx ON session_ratings (created_at);
