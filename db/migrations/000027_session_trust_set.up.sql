-- docs/adr/0086 §2: the Rule of Two letters a session may use, stored once at create from its trigger's tier.
-- A: untrusted trigger, B: sensitive reads, C: effects. ABC is a supervised session (an owner-chosen card option).
-- Existing sessions are owner or fleet triggered: BC.
ALTER TABLE sessions ADD COLUMN trust_set text NOT NULL DEFAULT 'BC'
  CHECK (trust_set IN ('BC', 'AB', 'AC', 'ABC'));

-- One-way: an UPDATE may drop letters, never add one. Only a new session gets a new set.
CREATE FUNCTION sessions_trust_set_narrow() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF translate(NEW.trust_set, OLD.trust_set, '') <> '' THEN
    RAISE EXCEPTION 'sessions.trust_set only narrows: % -> %', OLD.trust_set, NEW.trust_set;
  END IF;
  -- A is a taint, not a capability: dropping it would relabel an untrusted session as trusted.
  IF position('A' in OLD.trust_set) > 0 AND position('A' in NEW.trust_set) = 0 THEN
    RAISE EXCEPTION 'sessions.trust_set only narrows: % -> %', OLD.trust_set, NEW.trust_set;
  END IF;
  RETURN NEW;
END $$;

CREATE TRIGGER sessions_trust_set_narrow BEFORE UPDATE OF trust_set ON sessions
  FOR EACH ROW EXECUTE FUNCTION sessions_trust_set_narrow();
