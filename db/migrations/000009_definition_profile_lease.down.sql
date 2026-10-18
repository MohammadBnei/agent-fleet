-- docs/adr/0064: back to 000008's "no definition session holds a lease".
-- Clearing the leases first revokes live definition pods' CoreService
-- credential, which is what the restored CHECK would demand anyway.
UPDATE sessions SET lease_id = NULL WHERE definition_name IS NOT NULL;
ALTER TABLE sessions
  DROP CONSTRAINT sessions_definition_profile_lease,
  DROP COLUMN profile_lease_id,
  ADD CONSTRAINT sessions_definition_no_lease CHECK (definition_name IS NULL OR lease_id IS NULL);
