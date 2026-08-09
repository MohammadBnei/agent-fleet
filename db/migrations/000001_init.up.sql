-- agent-fleet shared task queue + knowledge journal.
-- Applied by the dedicated `migration` image (see migration/Dockerfile) via
-- common-app-chart's hooks.migrate PreSync job, and by golang-migrate
-- directly in core's integration tests (see core/internal/dbtest).
--
-- This is migration 1 of N: it captures the schema's end state as of the
-- golang-migrate cutover (docs/adr/0030), not the historical sequence of
-- ALTERs that built up the old single db/schema.sql file. Every schema
-- change from here on is a new numbered migration file — never an edit to
-- this one or any other already-applied file.

CREATE TABLE tasks (
  id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  repo               TEXT NOT NULL,           -- 'dream-analyst' | 'vos-monolith' | ...
  description        TEXT NOT NULL,
  status             TEXT NOT NULL DEFAULT 'pending'
                       CHECK (status IN ('pending', 'claimed', 'running', 'done', 'failed', 'cancelled', 'failed_permanently')),
  discord_channel_id TEXT,   -- NULL for tasks created from the dashboard (no Discord thread)
  discord_thread_id  TEXT,
  claimed_by         TEXT,
  pr_url             TEXT,
  notes              TEXT,
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

  -- Crash recovery + resume (docs/adr/0016). session_id is the
  -- Postgres-durable pointer to the session's own Claude SDK session; the
  -- actual transcript lives on the worker's RWX PVC (CLAUDE_CONFIG_DIR).
  -- heartbeat_at drives stale-claim reclaim; lease_id guards the rare
  -- split-brain case where a reclaim raced a not-actually-dead worker.
  session_id         TEXT,
  retry_count        INT NOT NULL DEFAULT 0,
  last_error         TEXT,
  heartbeat_at       TIMESTAMPTZ,
  lease_id           UUID,

  -- Which model actually ran this task's session (core/internal/tasks
  -- store's SaveSessionID, called by the sidecar on the worker's behalf).
  model              TEXT,

  -- DashboardService.DeleteTask soft-deletes: a hard DELETE would violate
  -- transcript/e2e_sessions' REFERENCES tasks(id) (no cascade) the moment a
  -- task has any transcript history, which is effectively always.
  deleted_at         TIMESTAMPTZ,

  -- Pod-lifecycle state (PodPhase: created/scheduled/running/crashed/
  -- terminated), set by ReportPodEvents — distinct from `status` (business
  -- state). Lets the dashboard show worker-pod state without kubectl.
  pod_phase          TEXT,
  pod_message        TEXT,

  -- The session's current SDK permission mode ("default"|"plan"|
  -- "acceptEdits"|"bypassPermissions"|...), set alongside the
  -- 'permission_mode' transcript entry by DashboardService.SetPermissionMode.
  -- NULL until a session's first SetPermissionMode call.
  permission_mode    TEXT,

  -- When a human first asked to stop this task (DashboardService.Stop).
  -- Stop only posts a cooperative abort message; dispatch.Loop's
  -- grace-period sweep uses this to force-tear-down a pod that never
  -- honored it, surviving a core restart since it's Postgres-durable.
  stop_requested_at  TIMESTAMPTZ,

  -- Bumped on every transcript append for this task, so the idle-timeout
  -- sweep can tell "pod alive but nobody's talking to it" apart from real
  -- activity. Distinct from heartbeat_at, which only proves the pod itself
  -- is alive.
  last_active_at     TIMESTAMPTZ,

  -- Resolved once at task-creation time (DashboardService.CreateTask joins
  -- the operator's selected snippet texts) and stored alongside
  -- description — a frozen snapshot, not a live reference to snippets that
  -- might later be edited or deleted. No FK/join table by design.
  guidance           TEXT NOT NULL DEFAULT ''
);

CREATE INDEX tasks_repo_status_idx ON tasks (repo, status);

-- Append-only fleet knowledge journal (mirrors ai-devkit's JSON-event
-- pattern — avoids write-conflict issues a shared mutable doc would hit
-- across concurrent worker pods).
CREATE TABLE knowledge_journal (
  id         BIGSERIAL PRIMARY KEY,
  repo       TEXT,
  actor      TEXT NOT NULL,        -- 'dream-analyst-worker' | 'vos-monolith-worker' | 'bot'
  event_type TEXT NOT NULL,
  payload    JSONB NOT NULL DEFAULT '{}',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- On-demand e2e test environments: one row per requested/live/torn-down e2e
-- pod for a task — the single coordination point between the worker's tool
-- calls, the provisioner's reconcile loop, and the dashboard's kill action.
CREATE TABLE e2e_sessions (
  id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  task_id              UUID NOT NULL REFERENCES tasks(id),
  status               TEXT NOT NULL DEFAULT 'requested'
                         CHECK (status IN ('requested', 'running', 'failed', 'torn_down')),
  pod_name             TEXT,
  ingress_path         TEXT,
  kill_requested       BOOLEAN NOT NULL DEFAULT false,
  created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
  -- Observability only (which trigger caused a teardown: human vs agent
  -- kill_env vs terminal-status auto-teardown) — not a dedup mechanism.
  kill_idempotency_key TEXT
);

CREATE INDEX e2e_sessions_task_idx ON e2e_sessions (task_id);

-- The durable store for the agent/human conversation (docs/adr/0013). `seq`
-- is the per-task monotonic cursor; core computes it inside the same
-- transaction as the insert, guarded by
-- pg_advisory_xact_lock(hashtext(task_id::text)) so a human's reply and the
-- agent's own concurrent send_message call can't race the same seq.
CREATE TABLE transcript (
  task_id            UUID NOT NULL REFERENCES tasks(id),
  seq                BIGINT NOT NULL,
  "from"             TEXT NOT NULL,
  text               TEXT NOT NULL,
  -- 'question'/'answer': the AskUserQuestion MCP tool posts a 'question'
  -- and long-polls for the matching 'answer', submitted via the dashboard.
  -- 'system'/'assistant'/'user'/'result': the SDK's own raw message
  -- discriminants, relayed verbatim ("relay everything, let the UI decide").
  -- 'tool_call': PushToolTelemetry's sidecar-pushed summary.
  -- 'permission_mode': human-authored, from the dashboard's mode selector.
  -- 'permission_request'/'permission_response': the generalized canUseTool
  -- prompt-and-wait pair — 'permission_request' is worker-authored (a JSON
  -- {tool, input} payload posted by canUseTool), 'permission_response' is
  -- the dashboard's human-authored allow/deny decision, correlated back via
  -- reply_to_seq the same way 'answer' correlates to 'question'.
  type               TEXT
                       CHECK (type IN (
                         'discussion', 'approve', 'abort', 'question', 'answer',
                         'tool_call', 'system', 'assistant', 'user', 'result', 'permission_mode',
                         'permission_request', 'permission_response'
                       ) OR type IS NULL),
  idempotency_key    TEXT NOT NULL,
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

  -- Retry/DLQ for the Discord-relay side effect only — the transcript
  -- entry itself is already durable the moment this row commits; these
  -- track whether core has successfully posted it to the Discord thread.
  relayed_to_discord BOOLEAN NOT NULL DEFAULT false,
  relay_attempts     INT NOT NULL DEFAULT 0,
  relay_dead_letter  BOOLEAN NOT NULL DEFAULT false,
  relay_last_error   TEXT,

  -- The question/request-seq correlation: NULL for every entry except an
  -- 'answer'/'permission_response' replying to a specific
  -- 'question'/'permission_request' entry's own seq.
  reply_to_seq       BIGINT,

  PRIMARY KEY (task_id, seq)
);

CREATE INDEX transcript_task_seq_idx ON transcript (task_id, seq);

-- Enforces the actual idempotency guarantee: retrying an append with the
-- same (task_id, idempotency_key) must not double-post a message.
CREATE UNIQUE INDEX transcript_idempotency_idx ON transcript (task_id, idempotency_key);

-- Target-repo config, dashboard-editable (docs/adr/0028) — replaces a
-- hardcoded Go map so onboarding/editing a repo no longer needs a core
-- redeploy. No FK from tasks.repo: a removed repo shouldn't retroactively
-- break historical task rows.
CREATE TABLE repos (
  name        TEXT PRIMARY KEY,
  url         TEXT NOT NULL,
  base_branch TEXT NOT NULL DEFAULT '', -- '' means the provisioner defaults to "main"
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO repos (name, url, base_branch) VALUES
  ('dream-analyst', 'https://github.com/MohammadBnei/dream-analyst.git', ''),
  ('vos-monolith',  'https://github.com/MohammadBnei/vos-monolith.git', 'dev'),
  ('agent-fleet',   'https://github.com/MohammadBnei/agent-fleet.git', '');

-- Dashboard-editable, reusable guidance text that an operator optionally
-- attaches to a task at creation time — a task's base prompt is just its
-- own description; anything more is one of these, picked per task.
CREATE TABLE prompt_snippets (
  id                         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  name                       TEXT NOT NULL UNIQUE,
  text                       TEXT NOT NULL,
  created_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
  -- NULL means no suggestion. When a task is created with snippets that
  -- have this set, the first non-null suggestion is auto-applied to the
  -- task's permission_mode.
  suggested_permission_mode  TEXT
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
   'Write the code following your plan, and add or update tests — run the repo''s test suite and make it pass; if there is no test suite, add one first. Update docs if relevant. Then commit your changes with git yourself via Bash, push, and open the PR yourself: push your branch (git push -u origin <your branch>), then gh pr create --title "..." --body "..." against this task''s base branch. Your git identity is already configured — just run the commands. Confirm the PR actually exists afterward (e.g. gh pr view) before telling Mohammad it''s ready.',
   NULL);
