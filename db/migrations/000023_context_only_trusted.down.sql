-- Reverses 000023. A context-only entry written trusted since then becomes
-- untrusted again, as the CHECK requires: its session is tainted under the
-- old rule, which fails closed.
UPDATE transcript SET untrusted = true WHERE context_only AND NOT untrusted;
ALTER TABLE transcript
  ADD CONSTRAINT transcript_context_only_untrusted CHECK (NOT context_only OR untrusted);
