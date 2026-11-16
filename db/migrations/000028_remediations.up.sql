-- docs/adr/0086 §3: a catalogue selection core has schema-checked, rate-limited and denylist-checked, written as a
-- single-use row the remediation executor pulls by id and reports the outcome onto.
CREATE TABLE remediations (
  id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  responder   text        NOT NULL,
  -- What the rate limit keys on: <namespace>/<name>, or application/<name> for sync_app.
  target      text        NOT NULL,
  action      text        NOT NULL CHECK (action IN ('restart', 'scale', 'delete_pod', 'sync_app', 'patch')),
  params      jsonb       NOT NULL,
  -- pending -> pulled (single use) -> done | refused | lost (pulled, never reported)
  state       text        NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'pulled', 'done', 'refused', 'lost')),
  outcome     jsonb,
  created_at  timestamptz NOT NULL DEFAULT now(),
  pulled_at   timestamptz,
  finished_at timestamptz
);
CREATE INDEX remediations_target_idx ON remediations (target, created_at);
CREATE INDEX remediations_responder_idx ON remediations (responder, created_at);
