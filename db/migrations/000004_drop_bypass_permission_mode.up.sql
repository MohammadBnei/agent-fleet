-- docs/adr/0053 deletes the "bypassPermissions" mode: "auto" does the same
-- job without a launch profile, and the worker answers everything but rm/sudo
-- in it.
--
-- This is not cosmetic. core's validPermissionModes no longer accepts the
-- value, but that only guards new writes — a row still carrying it is what the
-- NEXT warm launches the pod in, and the SDK would be handed a mode the fleet
-- no longer reasons about.
UPDATE sessions SET permission_mode = 'auto' WHERE permission_mode = 'bypassPermissions';
