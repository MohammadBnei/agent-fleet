-- Which images a session's pod actually ran, recorded from
-- CreateWorkerPodResponse at dispatch, so the console can answer "which build
-- is this session on?".
--
-- Stored rather than derived from the provisioner's current defaults on read:
-- a session warmed before a fleet upgrade keeps running the older worker until
-- its next warm, and reporting today's default for it would quietly lie during
-- exactly the window someone is asking.
--
-- NOT NULL DEFAULT '' on purpose. These scan into plain Go strings, and a
-- nullable column read into a plain string fails the whole query, not the row
-- (that is how one NULL description emptied the entire session list).
ALTER TABLE sessions
  ADD COLUMN worker_image TEXT NOT NULL DEFAULT '',
  ADD COLUMN sidecar_image TEXT NOT NULL DEFAULT '';
