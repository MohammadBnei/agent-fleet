-- The flattened `body` still carries the human-facing summary, so dropping the
-- column loses detail but nothing a proposal needs to be acted on.
ALTER TABLE proposals DROP COLUMN payload;
