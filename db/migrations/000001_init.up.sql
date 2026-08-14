-- agent-fleet schema. Applied by the dedicated `migration` image (see
-- migration/Dockerfile) via common-app-chart's hooks.migrate PreSync job,
-- and by golang-migrate directly in core's integration tests (see
-- core/internal/dbtest).
--
-- This file replaces the previous 14 migrations outright (docs/adr/0048).
-- Migrating forward would have meant a migration full of DROPs for columns
-- added three migrations earlier, plus a permanent 15-file history
-- describing a schema that no longer exists. The database was purged
-- instead, which is only acceptable because this fleet had no data worth
-- keeping — do NOT treat that as precedent. From here on the rule from
-- docs/adr/0030 applies again unchanged: every schema change is a new
-- numbered migration, never an edit to this file.

-- ---------------------------------------------------------------------------
-- sessions
-- ---------------------------------------------------------------------------
-- A session is the durable unit (docs/adr/0048, completing the rename
-- docs/adr/0029 deferred). A pod is ephemeral compute attached to one on
-- demand; the row, its working directory and its SDK state all outlive the
-- pod, which is what makes a session resumable.
--
-- Note what is NOT here, because their absence is the decision:
--
--   status       -- there is no queue and no computable completion. Sessions
--                -- are polymorphic (a bug fix, a feature, an explanation),
--                -- so nothing can compute "done" — which is why the old
--                -- enum's 'done' value never acquired a writer in its
--                -- entire life. Liveness is pod_phase + DeriveLiveState.
--   heartbeat_at -- liveness reconciles against Kubernetes every 60s. A
--                -- heartbeat only ever proved a timer was still firing.
--   retry_count  -- nothing retries; a failed session is resumed by a human
--                -- who can read why it failed.
--   pr_url       -- its only writer never passed it. The branch is
--                -- discoverable from the session, so the dashboard links a
--                -- PR search rather than storing a column that can go stale.
--   guidance     -- snippets now prefill the message composer, so operator
--                -- guidance reaches the model as text a human sent, not as
--                -- an invisible wrapper the agent cannot see or question.
CREATE TABLE sessions (
  id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  repo              TEXT NOT NULL,

  -- Human-facing labels only. Deliberately never part of the prompt: a
  -- polymorphic session's title conveys nothing the first message doesn't
  -- say better, and a description the agent cannot see is a description
  -- that silently drifts from what it was actually asked to do.
  title             TEXT,
  description       TEXT,

  -- The Agent SDK's own session id, for `resume:`. Named agent_session_id
  -- rather than session_id now that the row itself is a session. Written
  -- under a lease guard (see lease_id) and NULL until the SDK emits its
  -- first init message.
  agent_session_id  TEXT,
  model             TEXT,

  -- Minted per pod. Teardown is not synchronous and the workspace is
  -- shared, so a torn-down pod still finishing its shutdown must not be
  -- able to overwrite the resume identity of the pod that replaced it.
  -- Also gates the pre-push StillHoldsLease check.
  lease_id          UUID,

  -- The session's SDK permission mode ("default"|"plan"|"acceptEdits"|
  -- "bypassPermissions"|...). Restored on every warm — without it a
  -- session a human put into acceptEdits silently reverts to default.
  --
  -- NOT NULL DEFAULT 'default' rather than a bare nullable column: "sessions
  -- start in default mode, CLI parity" is a locked decision (docs/adr/0029),
  -- and a nullable column makes that true only as long as every reader
  -- remembers to coalesce NULL to "default". One that forgets hands the SDK
  -- an undefined mode. Stating it once here is the same guarantee with
  -- nothing to forget.
  permission_mode   TEXT NOT NULL DEFAULT 'default',

  -- Pod lifecycle, written by ReportPodEvents and by core's reconcile
  -- loop. This is the fleet's only liveness signal.
  pod_phase         TEXT,
  pod_message       TEXT,
  last_error        TEXT,

  -- Activity clock, written on every transcript append. Feeds the idle
  -- sweep, the retention GC, and DeriveLiveState.
  last_active_at    TIMESTAMPTZ,
  last_entry_type   TEXT,
  last_entry_from   TEXT,

  -- A real latch, reset when a pod is provisioned — not derivable from the
  -- transcript, because a resumed session already has agent-authored
  -- entries, so a derived version is permanently true and the startup-stall
  -- sweep would never fire again for a warmed session.
  activity_seen     BOOLEAN NOT NULL DEFAULT false,
  seen_at           TIMESTAMPTZ,

  -- When a human first asked to stop. Stop posts a cooperative abort; the
  -- grace sweep uses this to force-tear-down a pod that never honored it.
  -- The only thing that can kill a runaway agent, since a runaway keeps
  -- appending entries and so keeps last_active_at moving.
  stop_requested_at TIMESTAMPTZ,

  -- Disk reclaimed: the session directory and SDK state are gone, so the
  -- session is readable but no longer resumable. Written only by core's GC,
  -- which is the single writer of that fact.
  swept_at          TIMESTAMPTZ,

  -- The human is finished with this session. The only non-GC terminal
  -- state, because it is the only one a machine cannot compute.
  archived_at       TIMESTAMPTZ,

  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX sessions_repo_idx ON sessions (repo);
-- The live-pod count the concurrency cap gates on, and the idle/stall
-- sweeps' hot query.
CREATE INDEX sessions_pod_phase_idx ON sessions (pod_phase) WHERE archived_at IS NULL;

-- ---------------------------------------------------------------------------
-- proposals
-- ---------------------------------------------------------------------------
-- Machine-initiated suggestions: Alertmanager webhooks and scheduled
-- audits. A separate table rather than a status on sessions, because a
-- proposal has no pod path at all — that is what makes "a machine may
-- propose, never open" structural rather than a SELECT that politely skips
-- a value (docs/adr/0048).
--
-- A human turns one into a session via OpenFromProposal, behind the same
-- Traefik basic-auth as the rest of the dashboard. That is the gate that
-- used to be ApproveProposal, and it matters most for infra-bootstrap,
-- whose sessions carry cluster access.
CREATE TABLE proposals (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  repo         TEXT NOT NULL,
  source       TEXT NOT NULL CHECK (source IN ('alert', 'audit')),

  -- Alertmanager fingerprint, or audit:<id>. See the partial index below.
  dedup_key    TEXT,

  title        TEXT NOT NULL,
  body         TEXT NOT NULL,

  -- Set when a human opens it. ON DELETE SET NULL rather than CASCADE: a
  -- deleted session should not erase the record that something proposed it.
  -- It must not be a plain FK with no action either — that is exactly the
  -- constraint that forced the old schema into soft deletes.
  session_id   UUID REFERENCES sessions(id) ON DELETE SET NULL,

  dismissed_at TIMESTAMPTZ,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Dedup while the proposal stands, NOT while it is un-opened. Keying it on
-- "has no session yet" would free the key the moment a human opens it, so a
-- 1-hour audit cadence whose session runs 3 hours would file three
-- proposals for the same thing. Archiving a session dismisses its proposal,
-- which is what re-arms the key.
CREATE UNIQUE INDEX proposals_dedup_idx ON proposals (repo, dedup_key)
  WHERE dedup_key IS NOT NULL AND dismissed_at IS NULL;

CREATE INDEX proposals_open_idx ON proposals (created_at)
  WHERE session_id IS NULL AND dismissed_at IS NULL;

-- ---------------------------------------------------------------------------
-- transcript
-- ---------------------------------------------------------------------------
-- The durable agent/human conversation (docs/adr/0013). `seq` is the
-- per-session monotonic cursor; core computes it inside the same
-- transaction as the insert, guarded by
-- pg_advisory_xact_lock(hashtext(session_id::text)) so a human's reply and
-- the agent's own concurrent send_message can't race the same seq.
--
-- ON DELETE CASCADE is new, and it is what lets soft deletes go: the old
-- schema carried deleted_at solely because transcript and e2e_sessions both
-- REFERENCEd tasks with no action, so a real DELETE was impossible the
-- moment a task had any history — which was always.
CREATE TABLE transcript (
  session_id      UUID NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  seq             BIGINT NOT NULL,
  "from"          TEXT NOT NULL,
  text            TEXT NOT NULL,

  -- 'question'/'answer': the AskUserQuestion MCP tool posts a 'question'
  -- and long-polls for the matching 'answer'.
  -- 'system'/'assistant'/'user'/'result': the SDK's own raw message
  -- discriminants, relayed verbatim.
  -- 'tool_call': PushToolTelemetry's sidecar-pushed summary.
  -- 'permission_mode': human-authored, from the dashboard's mode selector.
  -- 'permission_request'/'permission_response': the canUseTool
  -- prompt-and-wait pair, correlated by reply_to_seq.
  --
  -- 'approve' is gone: the approval gate it belonged to was deleted in
  -- docs/adr/0029 and the value has had no writer since.
  type            TEXT
                    CHECK (type IN (
                      'discussion', 'abort', 'interrupt', 'question', 'answer',
                      'tool_call', 'system', 'assistant', 'user', 'result',
                      'permission_mode', 'permission_request', 'permission_response'
                    ) OR type IS NULL),

  idempotency_key TEXT NOT NULL,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

  -- NULL for every entry except an 'answer'/'permission_response' replying
  -- to a specific 'question'/'permission_request' entry's own seq.
  reply_to_seq    BIGINT,

  -- Whether a Discord notification has been posted for this entry. One
  -- nullable timestamp replaces the old four-column relay/dead-letter
  -- machine: Discord is now notification-with-a-deep-link, not a second
  -- console, so there is nothing to retry into and nothing to dead-letter.
  notified_at     TIMESTAMPTZ,

  PRIMARY KEY (session_id, seq)
);

-- Enforces the actual idempotency guarantee: retrying an append with the
-- same (session_id, idempotency_key) must not double-post.
CREATE UNIQUE INDEX transcript_idempotency_idx ON transcript (session_id, idempotency_key);

-- Finds unanswered questions and pending permission requests. This is what
-- replaces the old awaiting_human boolean, which could not be correct:
-- permissions are a LIST (parallel tool calls each get their own seq), but
-- the column was one bool, so answering any one of them cleared "blocked"
-- for all the others.
CREATE INDEX transcript_pending_idx ON transcript (session_id, type, seq)
  WHERE type IN ('question', 'permission_request');

-- ---------------------------------------------------------------------------
-- repos
-- ---------------------------------------------------------------------------
-- Target-repo config, dashboard-editable (docs/adr/0028). No FK from
-- sessions.repo: removing a repo shouldn't retroactively break historical
-- session rows.
CREATE TABLE repos (
  name           TEXT PRIMARY KEY,
  url            TEXT NOT NULL,
  base_branch    TEXT NOT NULL DEFAULT '', -- '' means the provisioner defaults to "main"

  -- The container image this repo's sessions run. '' means the fleet
  -- default. Replaces repo_profiles/repo_profile_tools' four toolchain
  -- ingredients (go-toolchain, bun-toolchain, golangci-lint, buf), which
  -- existed only because the worker image was generic and the sandbox was
  -- a separate pod — an init container copying a Go toolchain onto an
  -- emptyDir is an elaborate way of saying "this repo needs Go".
  image          TEXT NOT NULL DEFAULT '',

  -- Whether this repo's sessions get the kubectl shim that RPCs to
  -- thot-executor (docs/adr/0037). This is the ONE ingredient that did not
  -- collapse into `image`, because it is not a toolchain — it is a
  -- privilege grant, and the pod still holds zero Kubernetes credentials
  -- either way.
  --
  -- Kept as data rather than code for the same reason docs/adr/0037 gave:
  -- which sessions may reach the cluster is a human's decision, editable
  -- without a redeploy. A boolean rather than a profile row because there
  -- is exactly one such privilege and it is not composable with anything.
  cluster_access BOOLEAN NOT NULL DEFAULT false,

  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO repos (name, url, base_branch, cluster_access) VALUES
  ('dream-analyst',   'https://github.com/MohammadBnei/dream-analyst.git',   '',     false),
  ('vos-monolith',    'https://github.com/MohammadBnei/vos-monolith.git',    'dev',  false),
  ('agent-fleet',     'https://github.com/MohammadBnei/agent-fleet.git',     '',     false),
  -- thot sessions are ordinary sessions on the repo that holds the
  -- cluster's own IaC, so a durable fix is a normal PR against it.
  ('infra-bootstrap', 'https://github.com/MohammadBnei/infra-bootstrap.git', 'main', true);

-- ---------------------------------------------------------------------------
-- prompt_snippets
-- ---------------------------------------------------------------------------
-- Dashboard-editable, reusable guidance an operator attaches to a session.
-- These now prefill the message composer rather than being concatenated
-- into a hidden `guidance` column: the agent sees them as part of the
-- message a human sent, which is both honest and editable in the moment.
CREATE TABLE prompt_snippets (
  id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  name                      TEXT NOT NULL UNIQUE,
  text                      TEXT NOT NULL,
  -- NULL means no suggestion. When a session is opened with snippets that
  -- set this, the first non-null suggestion is applied to permission_mode.
  suggested_permission_mode TEXT,
  created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at                TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO prompt_snippets (name, text, suggested_permission_mode) VALUES
  ('Architecture interview',
   'If this task involves an architecturally non-trivial decision (new component, schema/protocol/API shape, a one-way door — see the architecture-interview skill''s own DOOR classification), type "/architecture-interview" as your next message to run it before you plan. Otherwise say in one line why it''s skipped and move on.',
   NULL),
  ('Post a plan for approval',
   'Before implementing, post your plan via send_message (from="agent"), citing the specific files/paths you read and relied on. If Mohammad has put this session into plan mode (the dashboard''s mode picker), call ExitPlanMode with that same plan once you''re confident — it blocks for a real human decision the same way it does in Claude Code''s own plan mode. Outside plan mode, just proceed to implementing once you''ve posted the plan.',
   'plan'),
  ('Doubt-driven review',
   'Once you have a plan, if it involves a non-trivial decision per the doubt-driven-development skill''s own "When to Use" checklist, type "/doubt-driven-development" as your next message to run it. Otherwise say why it''s skipped and move on. Its cross-model escalation step is non-interactive in this environment — skip it and announce the skip.',
   NULL),
  ('Open a PR when done',
   'Write the code following your plan, and add or update tests — run the repo''s test suite and make it pass; if there is no test suite, add one first. Update docs if relevant. Then commit your changes with git yourself via Bash, push, and open the PR yourself: create your branch (git checkout -b <name>), push it (git push -u origin <name>), then gh pr create --title "..." --body "..." against this session''s base branch. Your git identity is already configured — just run the commands. Confirm the PR actually exists afterward (e.g. gh pr view) before telling Mohammad it''s ready.',
   NULL);

-- ---------------------------------------------------------------------------
-- scheduled_audits
-- ---------------------------------------------------------------------------
-- Dashboard-editable periodic audits thot runs against the cluster. These
-- now file proposals rather than creating tasks directly, so the human gate
-- is the table they land in.
CREATE TABLE scheduled_audits (
  id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  name             TEXT NOT NULL UNIQUE,
  prompt           TEXT NOT NULL,
  -- Interval seconds rather than a cron expression: no cron parser exists
  -- anywhere in this codebase, and every audit anyone has actually asked
  -- for is "every N hours".
  interval_seconds INT NOT NULL CHECK (interval_seconds >= 60),
  enabled          BOOLEAN NOT NULL DEFAULT true,
  -- The scheduler's cursor. next_run_at is what the loop queries on;
  -- last_run_at is for the UI.
  next_run_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_run_at      TIMESTAMPTZ,
  last_status      TEXT,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_scheduled_audits_due ON scheduled_audits (next_run_at) WHERE enabled;

-- ---------------------------------------------------------------------------
-- knowledge_journal
-- ---------------------------------------------------------------------------
-- Append-only fleet memory. Deliberately not FK'd to sessions: it outlives
-- them, and a deleted session should not erase what was learned.
CREATE TABLE knowledge_journal (
  id         BIGSERIAL PRIMARY KEY,
  repo       TEXT,
  actor      TEXT NOT NULL,
  event_type TEXT NOT NULL,
  payload    JSONB NOT NULL DEFAULT '{}',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Full-text search backing the journal_search MCP tool. Postgres
-- to_tsvector/ts_rank — no new dependency, no embedding model, no vector
-- store.
CREATE INDEX knowledge_journal_fts_idx ON knowledge_journal
  USING GIN (to_tsvector('english', event_type || ' ' || payload::text));
