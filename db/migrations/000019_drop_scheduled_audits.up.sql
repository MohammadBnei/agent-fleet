-- The drop 000005 promised for "000006, in a later release" and never made:
-- 000006 became proposal_payload. No core since 000005's release reads
-- scheduled_audits (schedules replaced it, and 000005 copied every row
-- across), so the reason 000005 gave for waiting — the previous core still
-- running through the PreSync window — no longer applies: the core this
-- runs ahead of never touches the table.
DROP TABLE IF EXISTS scheduled_audits;
