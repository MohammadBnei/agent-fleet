-- docs/adr/0082 §5: one row per decision the work waits on the owner for. A
-- question, a permission request and a plan approval are projected here from
-- the transcript, inside the append's own transaction
-- (core/internal/decisions.OnAppend); a card is the fourth kind and comes with
-- draft_card. pending_decisions and the idle sweep read the open rows.
CREATE TABLE decisions (
  id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  kind          TEXT        NOT NULL CHECK (kind IN ('question', 'permission', 'plan')),
  session_id    UUID        NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
  -- The request's own transcript seq; its reply names it in reply_to_seq.
  request_seq   BIGINT      NOT NULL,
  state         TEXT        NOT NULL DEFAULT 'open'
                CHECK (state IN ('open', 'answered', 'withdrawn', 'expired', 'stale')),
  -- At most 280 characters, an agent claim.
  ask           TEXT        NOT NULL DEFAULT '',
  -- The request's text: JSON as the worker wrote it, or a JSON string when it is not JSON.
  payload       JSONB,
  -- A permission's or plan's tool, and its input as canonical JSON (decisions.Canon), which the one-time grant
  -- matches a retried call against.
  tool          TEXT,
  input_canon   TEXT,
  grantable     BOOLEAN     NOT NULL DEFAULT false,
  answer        JSONB,
  answered_by   TEXT,
  answered_at   TIMESTAMPTZ,
  grant_used_at TIMESTAMPTZ,
  -- Bumped on every change, from one sequence: WatchDecisions sends what moved past a version.
  version       BIGSERIAL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (session_id, request_seq)
);
CREATE INDEX decisions_state_version_idx ON decisions (state, version);

-- Backfill: every request still unanswered on a session that is neither
-- archived nor swept. A question outlives its pod, so it stays open; a
-- permission or plan is bound to its pod's canUseTool promise, so it is open
-- only while that pod lives (the next warm would close it anyway).
-- input_canon stays NULL: it is computed in Go, and these requests predate
-- one-time grants.
CREATE FUNCTION pg_temp.decisions_jsonb(t TEXT) RETURNS JSONB LANGUAGE plpgsql AS $$
BEGIN
  RETURN t::jsonb;
EXCEPTION WHEN others THEN
  RETURN to_jsonb(t);
END $$;

INSERT INTO decisions (kind, session_id, request_seq, state, ask, payload, tool, created_at)
SELECT k.kind, q.session_id, q.seq,
       CASE WHEN k.kind = 'question'
              OR s.pod_phase IN ('POD_PHASE_PROVISIONING', 'POD_PHASE_CREATED', 'POD_PHASE_SCHEDULED', 'POD_PHASE_RUNNING')
            THEN 'open' ELSE 'withdrawn' END,
       left(COALESCE(CASE k.kind
                       WHEN 'question' THEN p.payload -> 'questions' -> 0 ->> 'question'
                       WHEN 'plan' THEN p.payload -> 'input' ->> 'plan'
                       ELSE p.payload ->> 'tool'
                     END, ''), 280),
       p.payload,
       CASE WHEN k.kind <> 'question' THEN p.payload ->> 'tool' END,
       q.created_at
  FROM transcript q
  JOIN sessions s ON s.id = q.session_id
 CROSS JOIN LATERAL (SELECT pg_temp.decisions_jsonb(q.text) AS payload) p
 CROSS JOIN LATERAL (SELECT CASE WHEN q.type = 'question' THEN 'question'
                                 WHEN p.payload ->> 'tool' = 'ExitPlanMode' THEN 'plan'
                                 ELSE 'permission' END AS kind) k
 WHERE q.type IN ('question', 'permission_request')
   AND s.archived_at IS NULL
   AND s.swept_at IS NULL
   AND NOT EXISTS (
         SELECT 1 FROM transcript r
          WHERE r.session_id = q.session_id AND r.reply_to_seq = q.seq
       );
