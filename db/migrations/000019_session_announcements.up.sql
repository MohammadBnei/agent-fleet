-- docs/adr/0078 §3: the last waiting/resumed edge core got the event bridge to
-- accept, per session. Current state, like pod_phase, not a queue: the
-- announcer compares it with the session's pending decisions on every nudge
-- and tick, and publishes again only when they differ.
CREATE TABLE session_announcements (
  session_id   UUID        PRIMARY KEY REFERENCES sessions (id) ON DELETE CASCADE,
  waiting      BOOLEAN     NOT NULL,
  position     BIGINT      NOT NULL,
  published_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
