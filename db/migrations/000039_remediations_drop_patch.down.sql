ALTER TABLE remediations DROP CONSTRAINT remediations_action_check;
ALTER TABLE remediations ADD CONSTRAINT remediations_action_check
  CHECK (action IN ('restart', 'scale', 'delete_pod', 'sync_app', 'patch'));
