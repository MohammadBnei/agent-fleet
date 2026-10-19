-- docs/adr/0064. Dropping the column revokes live definition pods' CoreService
-- credential; 000008's "no definition session holds a lease" was never lifted.
ALTER TABLE sessions DROP CONSTRAINT sessions_profile_lease_definition_only;
ALTER TABLE sessions DROP COLUMN profile_lease_id;
