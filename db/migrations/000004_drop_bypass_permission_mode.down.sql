-- Deliberately not the inverse. The up migration is lossy — a session already
-- in 'auto' before it ran is indistinguishable afterwards from one that was in
-- 'bypassPermissions' — so mapping every 'auto' row back would put sessions
-- into a mode their human never chose. Rolling back the schema is safe here
-- precisely because 'auto' is the weaker of the two.
SELECT 1;
