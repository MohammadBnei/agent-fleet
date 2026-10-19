-- docs/adr/0064. A definition session's lease lives in its own column, never
-- in lease_id: lease_id is what every older core mints AND verifies, and an
-- older core knows nothing of definitions. Minting is already closed by
-- 000008's sessions_definition_no_lease (kept): an older core's ReserveSlot
-- would warm a definition session as a repo session (GH_TOKEN and LEASE_ID in
-- the worker, the whole repo cache, any mode), and the CHECK refuses its write.
-- Verifying is closed by this column: an older core (image rollback, a
-- RollingUpdate overlap) matches only lease_id, so a live definition pod's
-- lease authenticates nothing there and never reaches a core without the
-- definition allowlist. Only a profile-aware core's ReserveSlot writes it.
--
-- Revoking is the third half, and an older core cannot do it: its ClearLease
-- nulls lease_id only. So a profile-aware core accepts profile_lease_id only
-- while pod_phase is live (sessions.Store.VerifyLease), and the TERMINATED the
-- provisioner reports for any core's teardown revokes it.
--
-- The CHECK is the other direction of 000008's: a repo session never holds a
-- definition lease, which would verify with no allowlist applied.
ALTER TABLE sessions
  ADD COLUMN profile_lease_id UUID,
  ADD CONSTRAINT sessions_profile_lease_definition_only CHECK (definition_name IS NOT NULL OR profile_lease_id IS NULL);
