-- An AgentService thread an admin locked (docs/adr/0067 §6): who locked it,
-- by email. NULL = unlocked. While set, only members of platform-admins post
-- to or answer in the thread; everyone who could read it still can.
ALTER TABLE sessions ADD COLUMN locked_by TEXT;
