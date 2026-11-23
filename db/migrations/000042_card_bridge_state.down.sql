DROP TRIGGER decisions_never_reopen ON decisions;
DROP FUNCTION decisions_never_reopen();
DROP INDEX decisions_card_unpublished_idx;
ALTER TABLE decisions DROP COLUMN bridge_state, DROP COLUMN bridge_parked;
