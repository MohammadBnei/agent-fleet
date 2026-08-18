-- Irreversible by construction: the up migration deletes rows, and the data
-- it removed is not recoverable from anything else in this database. A no-op
-- down is the honest statement of that, not an oversight — golang-migrate
-- needs the file to exist to step the version back.
--
-- Rolling back the code alone is enough: applyPodEvent simply starts writing
-- pod.* rows again from that point on.
SELECT 1;
