-- docs/adr/0084 §9: when the live pod was created, so a character is replaced before the vault token cap.
ALTER TABLE sessions ADD COLUMN pod_started_at TIMESTAMPTZ;
-- Characters already running have no start time: treat their pod as at the cap so each is replaced once, through
-- the drain, rather than killed undrained at its deadline. A row without a pod is restamped when its next pod is created.
UPDATE sessions SET pod_started_at = now() - interval '12 hours' WHERE lifecycle = 'standing' AND stopped_at IS NULL AND archived_at IS NULL;
