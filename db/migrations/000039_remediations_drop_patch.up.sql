-- docs/adr/0086 amendment: 'patch' is removed. The executor never implemented it, so a queued patch could only be
-- refused at pull time. A patch row not yet final ends refused here, then the CHECK stops admitting the action.
UPDATE remediations
   SET state = 'refused', finished_at = now(),
       outcome = '{"status":"refused","detail":"patch removed from the catalogue"}'::jsonb
 WHERE action = 'patch' AND state IN ('pending', 'pulled');
ALTER TABLE remediations DROP CONSTRAINT remediations_action_check;
-- NOT VALID: finished patch rows stay as history; every new or updated row is checked.
ALTER TABLE remediations ADD CONSTRAINT remediations_action_check
  CHECK (action IN ('restart', 'scale', 'delete_pod', 'sync_app')) NOT VALID;
