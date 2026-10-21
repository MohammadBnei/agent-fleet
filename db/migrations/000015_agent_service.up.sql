-- docs/adr/0066, 0067 and 0076: the AgentService schema. Nothing writes the
-- new columns or tables yet except the answer index below; the handlers come
-- in the next PRs.

-- ---------------------------------------------------------------------------
-- sessions: owner and per-thread choices (docs/adr/0067 §1)
--
-- Existing rows keep NULL owner and visibility, which read as admin-only, so
-- no P1-P3 test session ever becomes visible to an audience. NULL
-- interaction and lifecycle read as 'ask' and 'resident'.
ALTER TABLE sessions
  ADD COLUMN owner         TEXT,
  ADD COLUMN visibility    TEXT CHECK (visibility IN ('shared', 'private')),
  ADD COLUMN invoker       TEXT,
  ADD COLUMN interaction   TEXT CHECK (interaction IN ('ask', 'deny')),
  ADD COLUMN lifecycle     TEXT CHECK (lifecycle IN ('resident', 'single-shot')),
  ADD COLUMN thread_key    TEXT,
  -- The first context-only entry appended while the session had no pod
  -- (docs/adr/0067 §3); the next warm delivers from here and clears it.
  ADD COLUMN cold_from_seq BIGINT,
  -- The last pod event's reason (docs/adr/0066 §10). Status is derived from
  -- it; there is still no status column (docs/adr/0048 §2).
  ADD COLUMN pod_reason    TEXT,
  ADD CONSTRAINT sessions_agent_fields_definition_only CHECK (
    definition_name IS NOT NULL
    OR (owner IS NULL AND visibility IS NULL AND invoker IS NULL
        AND interaction IS NULL AND lifecycle IS NULL AND thread_key IS NULL)
  ),
  -- A client-opened session is owned by that client, and only a client has
  -- threads.
  ADD CONSTRAINT sessions_invoker_owns CHECK (invoker IS NULL OR (owner IS NOT NULL AND owner = 'client:' || invoker)),
  ADD CONSTRAINT sessions_thread_key_invoker CHECK (thread_key IS NULL OR invoker IS NOT NULL);

-- At most one open thread per (client, definition, key): Open's resume target
-- (docs/adr/0066 §6). "Open" is derived from archived_at, never stored.
CREATE UNIQUE INDEX sessions_open_thread_idx ON sessions (invoker, definition_name, thread_key)
  WHERE archived_at IS NULL AND thread_key IS NOT NULL;

-- ---------------------------------------------------------------------------
-- transcript: authorship and trust (docs/adr/0067 §3)
ALTER TABLE transcript
  ADD COLUMN author_sub   TEXT,
  ADD COLUMN author_name  TEXT,
  ADD COLUMN actor        TEXT,
  ADD COLUMN causation_id TEXT,
  ADD COLUMN context_only BOOLEAN NOT NULL DEFAULT false,
  ADD COLUMN untrusted    BOOLEAN NOT NULL DEFAULT false,
  -- A context-only entry is untrusted (docs/adr/0067 §3).
  ADD CONSTRAINT transcript_context_only_untrusted CHECK (NOT context_only OR untrusted);

-- One answer per prompt (docs/adr/0066 §4): two members answering at once get
-- one answer and one AlreadyExists. Live data holds 4 earlier pairs
-- (2026-10-21: a plan approval into auto, where the worker records the mode
-- switch's resolution and the dashboard then answers the same seq). The later
-- copy is unlinked, not deleted: its seq stays taken, so MAX(seq)+1 never
-- reuses a number a live cursor has passed, and a response with no
-- reply_to_seq is skipped by every reader.
UPDATE transcript t
   SET reply_to_seq = NULL
  FROM transcript first
 WHERE t.type IN ('answer', 'permission_response')
   AND first.type IN ('answer', 'permission_response')
   AND t.session_id = first.session_id
   AND t.reply_to_seq = first.reply_to_seq
   AND first.seq < t.seq;

CREATE UNIQUE INDEX transcript_one_reply_idx ON transcript (session_id, reply_to_seq)
  WHERE reply_to_seq IS NOT NULL AND type IN ('answer', 'permission_response');

-- ---------------------------------------------------------------------------
-- client_requests: idempotency claims for Open and Invoke (docs/adr/0066 §7)
--
-- Never deleted, so a key stays used across archive. session_id is NULL
-- until step 4 resolves one, and outlives a deleted session.
CREATE TABLE client_requests (
  invoker         TEXT        NOT NULL,
  idempotency_key TEXT        NOT NULL,
  session_id      UUID        REFERENCES sessions(id) ON DELETE SET NULL,
  request_sha     TEXT        NOT NULL,
  state           TEXT        NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'done')),
  claimed_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (invoker, idempotency_key)
);

-- For the ON DELETE SET NULL when a session row is deleted.
CREATE INDEX client_requests_session_idx ON client_requests (session_id) WHERE session_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Grants: remembered approvals (docs/adr/0076 §3). Core renders the rule from
-- the parked request; nothing here is answerer text.
CREATE TABLE session_grants (
  session_id UUID        NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  rule       TEXT        NOT NULL,
  granted_by TEXT        NOT NULL,
  granted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (session_id, rule)
);

-- Keyed by name, not (name, sha): a grant follows the definition across
-- commits. Revoked rows stay as the record of who granted what.
CREATE TABLE definition_grants (
  id              BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  definition_name TEXT        NOT NULL,
  rule            TEXT        NOT NULL,
  granted_by      TEXT        NOT NULL,
  granted_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  revoked_at      TIMESTAMPTZ
);

CREATE UNIQUE INDEX definition_grants_live_idx ON definition_grants (definition_name, rule)
  WHERE revoked_at IS NULL;
