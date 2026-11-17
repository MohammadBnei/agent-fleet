-- docs/adr/0084 §9: the owner's raise_budget answer lifts a character's daily turn hold until this time (24h).
ALTER TABLE sessions ADD COLUMN budget_override_until TIMESTAMPTZ;
