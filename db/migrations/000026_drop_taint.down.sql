-- The labels are not restored: every proposal and decision comes back untainted,
-- and only context-only entries come back untrusted, as 000015's constraint needs.
ALTER TABLE transcript ADD COLUMN untrusted BOOLEAN NOT NULL DEFAULT false;
UPDATE transcript SET untrusted = true WHERE context_only;
ALTER TABLE transcript ADD CONSTRAINT transcript_context_only_untrusted CHECK (NOT context_only OR untrusted);
ALTER TABLE dispatch_decisions ADD COLUMN tainted BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE proposals ADD COLUMN tainted BOOLEAN NOT NULL DEFAULT false;
