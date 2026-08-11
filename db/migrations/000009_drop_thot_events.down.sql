-- Recreates the schema only. The rows dropped by the up migration CANNOT
-- be restored — see its comment.
CREATE TABLE IF NOT EXISTS thot_events (
  id              BIGSERIAL PRIMARY KEY,
  kind            TEXT NOT NULL
                    CHECK (kind IN ('permission_request', 'permission_response',
                                    'finding', 'alert', 'audit_run')),
  actor           TEXT NOT NULL,
  payload         TEXT NOT NULL,
  reply_to        BIGINT REFERENCES thot_events(id),
  idempotency_key TEXT NOT NULL UNIQUE,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_thot_events_id ON thot_events (id);
CREATE INDEX IF NOT EXISTS idx_thot_events_reply_to ON thot_events (reply_to) WHERE reply_to IS NOT NULL;
