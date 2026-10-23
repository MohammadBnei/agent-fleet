-- docs/adr/0078 §8: what PublishEvent needs from the dispatcher's decisions.
-- hops is the delivered event's own, so an event a woken session publishes is
-- one hop further; thread and by_agent let the gate cap the wakes PublishEvent
-- causes per thread, since hops do not survive a GitHub round-trip.
ALTER TABLE dispatch_decisions
  ADD COLUMN thread   TEXT,
  ADD COLUMN hops     INT     NOT NULL DEFAULT 0,
  ADD COLUMN by_agent BOOLEAN NOT NULL DEFAULT false;
CREATE INDEX dispatch_decisions_agent_wakes_idx ON dispatch_decisions (definition, thread, decided_at)
  WHERE by_agent AND wake;

-- Every event a session published through PublishEvent: the request's
-- identity, so a reused idempotency key with another request is refused
-- rather than silently dropped by the bridge's dedup, and a per-session
-- budget bounds how wide an agent can fan out.
CREATE TABLE published_events (
  session_id   UUID        NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
  event_id     TEXT        NOT NULL,
  request_sha  TEXT        NOT NULL,
  published_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (session_id, event_id)
);
CREATE INDEX published_events_recent_idx ON published_events (session_id, published_at);
