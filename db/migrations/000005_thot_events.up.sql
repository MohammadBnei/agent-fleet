-- thot's own activity stream (docs/adr/0035). Deliberately NOT reused from
-- `transcript`: that table is keyed (task_id, seq) with a FK to tasks(id),
-- and most of what thot does doesn't originate from a task at all (a
-- scheduled audit, an Alertmanager alert, a human question). Forcing a
-- synthetic task row per audit would pollute the task list and the
-- dispatch loop's own queries.
--
-- Unlike `transcript`, ordering is a plain BIGSERIAL rather than a
-- per-scope seq computed under an advisory lock — there's exactly one
-- global thot stream, and Postgres sequences are already atomic, so the
-- lock transcript needs for gapless per-task seq assignment buys nothing
-- here.
--
-- ponytail: a BIGSERIAL cursor can technically skip a row whose
-- transaction commits after a later id became visible, so a poller could
-- miss it. Not reachable today — thot is single-replica and serializes
-- its own work through one FIFO queue, so appends don't overlap. Revisit
-- (a commit-ordered cursor, or LISTEN/NOTIFY) if thot ever runs >1
-- replica.
CREATE TABLE thot_events (
  id              BIGSERIAL PRIMARY KEY,

  -- 'permission_request'/'permission_response': the canUseTool prompt-and-
  -- wait pair, same shape transcript's own pair uses — 'permission_request'
  -- is thot-authored (a JSON {tool, input} payload), 'permission_response'
  -- is the dashboard's human-authored allow/deny, correlated via reply_to.
  -- 'finding': something thot concluded and wants on the record.
  -- 'alert': an inbound Alertmanager alert that triggered investigation.
  -- 'audit_run': a scheduled audit firing.
  kind            TEXT NOT NULL
                    CHECK (kind IN (
                      'permission_request', 'permission_response',
                      'finding', 'alert', 'audit_run'
                    )),
  actor           TEXT NOT NULL, -- 'thot' | 'human' | 'scheduler' | 'alertmanager'
  payload         TEXT NOT NULL,

  -- NULL for everything except a 'permission_response' answering a
  -- specific 'permission_request' row's id. Same correlation discipline as
  -- transcript.reply_to_seq — never "any pending request + any reply",
  -- which would let one decision satisfy an unrelated concurrent prompt.
  reply_to        BIGINT REFERENCES thot_events(id),

  idempotency_key TEXT NOT NULL UNIQUE,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The dashboard feed's only read pattern: everything since a cursor, in
-- id order.
CREATE INDEX idx_thot_events_id ON thot_events (id);

-- Resolving "is this request already answered" without scanning the whole
-- stream — the long-poll in CoreService.RequestThotPermission hits this on
-- every poll tick.
CREATE INDEX idx_thot_events_reply_to ON thot_events (reply_to) WHERE reply_to IS NOT NULL;
