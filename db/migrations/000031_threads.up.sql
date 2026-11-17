-- docs/adr/0084 §4, slice 4: threads inside one character. A standing row keeps one session row and one pod; each
-- thread is its own SDK session with its own cursor. Every transcript entry carries its thread; 'main' is the
-- owner's admin thread and every entry of a non-standing row.
ALTER TABLE transcript ADD COLUMN thread_key TEXT NOT NULL DEFAULT 'main';
CREATE INDEX transcript_thread_idx ON transcript (session_id, thread_key, seq);

CREATE TABLE session_threads (
  session_id       UUID NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  thread_key       TEXT NOT NULL CHECK (thread_key ~ '^[a-z0-9][a-z0-9-]{0,31}$'),
  -- First seq not yet done in this thread: advanced past a result's consumedSeq, so a re-warm resumes here and never
  -- from LatestSeq (§4). 'main' keeps its SDK session id in sessions.agent_session_id; every other thread here.
  cursor           BIGINT NOT NULL DEFAULT 0,
  agent_session_id TEXT,
  opened_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_active_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  -- Closing drops the SDK session id; the transcript stays. A later owner message reopens the key afresh.
  closed_at        TIMESTAMPTZ,
  -- The highest entry seq of this thread an effect method ran for: after a crash that entry is done, never replayed.
  effect_seq       BIGINT,
  -- The head entry the last crash settled on, and how many times in a row (a poison entry is done at three).
  crash_seq        BIGINT,
  crashes          INT NOT NULL DEFAULT 0,
  PRIMARY KEY (session_id, thread_key)
);

-- Standing rows started before this migration: their one thread, resumed from where the transcript ends.
INSERT INTO session_threads (session_id, thread_key, cursor)
SELECT s.id, 'main', COALESCE((SELECT MAX(t.seq) + 1 FROM transcript t WHERE t.session_id = s.id), 0)
  FROM sessions s WHERE s.lifecycle = 'standing';
