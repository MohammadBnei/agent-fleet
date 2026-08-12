-- Session liveness (docs/adr/0040): the inputs for deriving whether a
-- session is working, blocked, idle, done, stalled or unknown.
--
-- Deliberately columns of INPUTS, not a stored live_state enum. Every
-- input here is already written by a code path that exists (the
-- activityTrackingStore decorator that maintains last_active_at and
-- awaiting_human), so the state itself is a pure function of one tasks row
-- and cannot drift out of sync with the transcript the way a cached enum
-- would. `tasks.status` stays exactly what it is — a workflow status;
-- liveness is a second, orthogonal dimension.

-- What the newest transcript entry was. Enough to distinguish "the agent
-- closed the turn" (a result) from "a human said something and nothing has
-- come back yet" (the stalled case) without re-reading the transcript per
-- task, which is what the dashboard's client-side isCogitating heuristic
-- had to do.
ALTER TABLE tasks ADD COLUMN last_entry_type TEXT;
ALTER TABLE tasks ADD COLUMN last_entry_from TEXT;

-- False from the moment a pod is dispatched until that pod's agent posts
-- anything at all. This is the startup-stall signal: last_active_at can't
-- serve, because ClaimNextTask sets it to now() at claim time, so a pod
-- that comes up and never speaks looks "recently active" for the full
-- idle timeout (30 min) before anything notices.
ALTER TABLE tasks ADD COLUMN activity_seen BOOLEAN NOT NULL DEFAULT false;

-- When a human last opened this session's detail view. Distinguishes
-- "idle" from "done" — a session that finished while nobody was looking.
-- Only an explicit open sets this; the task list's poll deliberately does
-- not, or merely having the list on screen would mark everything seen.
ALTER TABLE tasks ADD COLUMN seen_at TIMESTAMPTZ;

-- Every pre-existing row predates the tracking above. Leaving
-- activity_seen false would make each of them look like a brand-new pod
-- that never spoke, and the startup-stall sweep would tear down any that
-- currently hold a live pod. Backfill from the transcript: a task with any
-- agent-authored entry has demonstrably seen activity.
UPDATE tasks SET activity_seen = true
WHERE EXISTS (
  SELECT 1 FROM transcript
  WHERE transcript.task_id = tasks.id AND transcript.from = 'agent'
);
