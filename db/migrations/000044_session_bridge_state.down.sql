SET lock_timeout = '3s';
ALTER TABLE sessions DROP COLUMN bridge_parked_at, DROP COLUMN bridge_turn_seq, DROP COLUMN bridge_state;
RESET lock_timeout;
