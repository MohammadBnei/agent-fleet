DROP TRIGGER sessions_trust_set_narrow ON sessions;
DROP FUNCTION sessions_trust_set_narrow();
ALTER TABLE sessions DROP COLUMN trust_set;
