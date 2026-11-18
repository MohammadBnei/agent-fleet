-- docs/adr/0084 amendment A4: the highest transcript seq when Start last resumed a stopped character. A Stop's abort
-- in main at or below it was meant for the earlier pod, and the resumed pod's stream skips it (clock-free).
ALTER TABLE sessions ADD COLUMN restart_seq BIGINT;
