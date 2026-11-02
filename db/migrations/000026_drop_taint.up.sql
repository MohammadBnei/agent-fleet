-- docs/adr/0082 §3 (owner, 2026-11-02): taint goes end to end, labels included.
-- An entry's author and actor still say who wrote it. Dropping transcript.untrusted
-- drops transcript_context_only_untrusted with it.
UPDATE proposals SET title = left(title, -length(' (untrusted)'))
 WHERE tainted AND title LIKE '% (untrusted)';
ALTER TABLE proposals DROP COLUMN tainted;
ALTER TABLE dispatch_decisions DROP COLUMN tainted;
ALTER TABLE transcript DROP COLUMN untrusted;
