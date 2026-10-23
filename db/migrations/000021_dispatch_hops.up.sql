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
