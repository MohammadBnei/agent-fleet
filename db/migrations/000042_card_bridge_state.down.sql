DROP INDEX decisions_card_unpublished_idx;
ALTER TABLE decisions DROP COLUMN bridge_state, DROP COLUMN bridge_parked;
