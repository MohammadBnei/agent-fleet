DROP INDEX IF EXISTS transcript_result_idx;
ALTER TABLE sessions DROP COLUMN bridge_parked, DROP COLUMN bridge_turn_seq, DROP COLUMN bridge_state;
