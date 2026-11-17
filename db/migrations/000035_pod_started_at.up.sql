-- docs/adr/0084 §9: when the live pod was created, so a character is replaced before the vault token cap.
ALTER TABLE sessions ADD COLUMN pod_started_at TIMESTAMPTZ;
