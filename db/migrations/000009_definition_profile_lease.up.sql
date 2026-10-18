-- docs/adr/0064. 000008 forbade a definition session any lease, so no core
-- could warm one. This core can (it sends the definition pod profile), an
-- older one still must not: it would warm it as a repo session (GH_TOKEN and
-- LEASE_ID in the worker, the whole repo cache, any mode). So a definition
-- session's lease must be the one ReserveSlot minted into profile_lease_id in
-- the same statement, which only a profile-aware core writes. An older core's
-- ReserveSlot mints a lease without it and this CHECK refuses the write.
-- IS NOT DISTINCT FROM, not =: a NULL profile_lease_id must fail the CHECK,
-- and `lease = NULL` is NULL, which a CHECK passes.
ALTER TABLE sessions
  ADD COLUMN profile_lease_id UUID,
  DROP CONSTRAINT sessions_definition_no_lease,
  ADD CONSTRAINT sessions_definition_profile_lease
    CHECK (definition_name IS NULL OR lease_id IS NULL OR lease_id IS NOT DISTINCT FROM profile_lease_id);
