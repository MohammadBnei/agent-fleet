-- docs/adr/0082 §4: the worker reads a session's taint (does it hold an
-- untrusted entry?) on every tool call it gates, until it is tainted. Without
-- this, each read walks the session's whole transcript.
CREATE INDEX transcript_untrusted_idx ON transcript (session_id) WHERE untrusted;
