-- agent-fleet shared task queue + knowledge journal.
-- Applied via common-app-chart's hooks.migrate PreSync job on the bot's Application.
-- Safe to re-run: every statement is idempotent.

CREATE TABLE IF NOT EXISTS tasks (
  id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  repo              TEXT NOT NULL,           -- 'dream-analyst' | 'vos-monolith'
  description       TEXT NOT NULL,
  status            TEXT NOT NULL DEFAULT 'pending',
  discord_channel_id TEXT NOT NULL,
  discord_thread_id  TEXT,
  claimed_by        TEXT,
  pr_url            TEXT,
  notes             TEXT,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS tasks_repo_status_idx ON tasks (repo, status);

-- skip_critique was the /task-time opt-out for the old proposer/critic
-- design (docs/adr/0011, superseded) — dead now that planning is a single
-- session and interview/doubt gating is the agent's own judgment call
-- (docs/adr/0017).
ALTER TABLE tasks DROP COLUMN IF EXISTS skip_critique;

-- Named + re-applied via DROP/ADD (not inline on the column) so adding a new
-- status later — like 'cancelled' below, for the round-cap/kill-switch
-- guardrails, and 'failed_permanently' (reliability-findings.md #1: a task
-- reclaimed past MAX_TASK_RETRIES stops retrying instead of looping
-- forever) — is a safe re-run against the already-live table, not just
-- future fresh creates.
--
-- 'planning'/'implementing' are gone as of the sessions redesign
-- (supersedes docs/adr/0021/0025's phase-boundary framing) — there's no
-- fleet-imposed phase left to track. 'running' replaces 'planning' as what
-- index.ts sets once a session's pod is actually live, with no
-- 'implementing' successor since there's no phase transition left to mark.
-- Clean cutover, not a tolerated-legacy-value migration: homelab-scale,
-- single deploy, no rolling-replica window where an old and new binary
-- would both be writing this column at once.
--
-- Existing rows still holding a pre-cutover phase status collapse to
-- 'running' *before* the stricter CHECK below is added — 'planning'
-- because the pod was live, 'implementing' because that implied the pod
-- was well past merely claimed. Must run first: adding the new CHECK
-- against rows that still hold the old values would fail immediately.
UPDATE tasks SET status = 'running' WHERE status IN ('planning', 'implementing');

ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_status_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_status_check
  CHECK (status IN ('pending', 'claimed', 'running', 'done', 'failed', 'cancelled', 'failed_permanently'));

-- Crash recovery + resume (see docs/adr/0016). session_id is the
-- Postgres-durable *pointer* to the session's own Claude SDK session
-- (docs/adr/0017 — one session, not a proposer_session_id/critic_session_id
-- pair) whose actual transcript now lives on the worker's RWX PVC
-- (CLAUDE_CONFIG_DIR, see ADR-0016) — the id alone is useless without that
-- PVC-side change. heartbeat_at drives stale-claim reclaim in
-- claimNextTask; lease_id guards the rare split-brain case where a reclaim
-- raced a not-actually-dead worker.
--
-- Renamed from planning_session_id as part of the sessions redesign
-- (supersedes docs/adr/0021/0025's phase-boundary framing) — a guarded
-- rename first (safe against an already-deployed environment that still
-- has the old column), then the usual ADD COLUMN IF NOT EXISTS covers a
-- genuinely fresh install.
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'tasks' AND column_name = 'planning_session_id')
     AND NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'tasks' AND column_name = 'session_id') THEN
    ALTER TABLE tasks RENAME COLUMN planning_session_id TO session_id;
  END IF;
END $$;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS session_id TEXT;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS retry_count INT NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS last_error TEXT;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS heartbeat_at TIMESTAMPTZ;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS lease_id UUID;

-- Which model actually ran this task's session — set alongside
-- session_id (core/internal/tasks/store.go's SaveSessionID, called by the
-- sidecar on the worker's behalf, docs/adr/0020/0021). Only new column the
-- shared-PVC/provisioner/sidecar redesign (docs/adr/0019-0021) actually
-- needed — confirmed by re-checking every query the rewritten Go services
-- issue against this table.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS model TEXT;

-- Tasks created from the dashboard (DashboardService.CreateTask) have no
-- Discord channel/thread at all — core/internal/discord/session.go's
-- PostToThread already no-ops when discord_thread_id is NULL, so relaxing
-- this is enough to support that origin without any other schema change.
ALTER TABLE tasks ALTER COLUMN discord_channel_id DROP NOT NULL;

-- DashboardService.DeleteTask soft-deletes: a hard DELETE would violate
-- transcript/e2e_sessions' REFERENCES tasks(id) (no cascade) the
-- moment a task has any transcript history, which is effectively always.
-- GetTask/ListRecentTasks both filter WHERE deleted_at IS NULL.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;

-- Pod-lifecycle state (PodPhase: created/scheduled/running/crashed/
-- terminated), set by ReportPodEvents alongside its existing knowledge_journal
-- write — distinct from `status` (business state: pending/claimed/running/
-- done/...). Lets the dashboard show worker-pod state directly instead of
-- requiring kubectl.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS pod_phase TEXT;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS pod_message TEXT;

-- The session's current SDK permission mode ("default"|"plan"|
-- "acceptEdits"|"bypassPermissions"|...) — set alongside the
-- 'permission_mode' transcript entry by DashboardService.SetPermissionMode,
-- so the dashboard's mode picker can highlight the real active mode
-- instead of inferring it from transcript history. NULL until a session's
-- first SetPermissionMode call (a fresh session starts in "default" inside
-- worker/src/session.ts, but that's not durable here until explicitly set).
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS permission_mode TEXT;

-- When a human first asked to stop this task (DashboardService.Stop).
-- Stop itself only posts a cooperative abort message to the transcript;
-- dispatch.Loop's grace-period sweep uses this timestamp to force-tear
-- down a worker pod that never noticed/honored it, surviving a core
-- restart since it's Postgres-durable rather than an in-memory timer.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS stop_requested_at TIMESTAMPTZ;

-- Substrate for the sessions redesign's idle-timeout backstop: bumped on
-- every transcript append for this task (a decorator around
-- transcript.Store, not every RPC call site individually), so a sweep can
-- tell "pod alive but nobody's talking to it" apart from real activity.
-- Distinct from heartbeat_at, which fires unconditionally on a timer and
-- only proves the pod is alive, not that a human is present.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS last_active_at TIMESTAMPTZ;

-- Append-only fleet knowledge journal (mirrors ai-devkit's JSON-event pattern,
-- see agent-fleet reference-check memory: avoids write-conflict issues that a
-- shared mutable doc would hit across concurrent worker pods).
CREATE TABLE IF NOT EXISTS knowledge_journal (
  id         BIGSERIAL PRIMARY KEY,
  repo       TEXT,
  actor      TEXT NOT NULL,        -- 'dream-analyst-worker' | 'vos-monolith-worker' | 'bot'
  event_type TEXT NOT NULL,
  payload    JSONB NOT NULL DEFAULT '{}',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- On-demand e2e test environments: one row per requested/live/torn-down e2e
-- pod for a task. The single coordination point between the worker's tool
-- calls, the e2e-provisioner's reconcile loop, and the bot's /e2e-kill
-- command — none of them talk to each other directly, only through this
-- table (+ tasks.status for the terminal-state teardown trigger).
CREATE TABLE IF NOT EXISTS e2e_sessions (
  id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  task_id        UUID NOT NULL REFERENCES tasks(id),
  status         TEXT NOT NULL DEFAULT 'requested',
  pod_name       TEXT,
  ingress_path   TEXT,
  kill_requested BOOLEAN NOT NULL DEFAULT false,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS e2e_sessions_task_idx ON e2e_sessions (task_id);

ALTER TABLE e2e_sessions DROP CONSTRAINT IF EXISTS e2e_sessions_status_check;
ALTER TABLE e2e_sessions ADD CONSTRAINT e2e_sessions_status_check
  CHECK (status IN ('requested', 'running', 'failed', 'torn_down'));

-- Observability only (which trigger caused a given teardown: human
-- /e2e-kill vs agent kill_env vs terminal-status auto-teardown) — not a
-- dedup mechanism, kill_requested=true is already idempotent to set twice.
ALTER TABLE e2e_sessions ADD COLUMN IF NOT EXISTS kill_idempotency_key TEXT;

-- Replaces the Redis list (agentfleet:planning:<taskId>) as the durable
-- store for the agent/human conversation (see docs/adr/0013). `seq` is the
-- per-task monotonic cursor that replicates LRANGE-from-index semantics —
-- fleet-core computes it inside the same transaction as the insert,
-- guarded by pg_advisory_xact_lock(hashtext(task_id::text)) so a human's
-- Discord reply and the agent's own concurrent send_message call can't
-- race the same seq.
--
-- Renamed from planning_transcript as part of the sessions redesign
-- (supersedes docs/adr/0021/0025's phase-boundary framing — there's no
-- distinct "planning" phase left to name the table after). Guarded rename
-- first for an already-deployed environment, then CREATE TABLE IF NOT
-- EXISTS covers a genuinely fresh install (a no-op once the rename above
-- already ran).
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'planning_transcript')
     AND NOT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'transcript') THEN
    ALTER TABLE planning_transcript RENAME TO transcript;
  END IF;
END $$;
CREATE TABLE IF NOT EXISTS transcript (
  task_id         UUID NOT NULL REFERENCES tasks(id),
  seq             BIGINT NOT NULL,
  "from"          TEXT NOT NULL,
  text            TEXT NOT NULL,
  type            TEXT,
  idempotency_key TEXT NOT NULL,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (task_id, seq)
);

DROP INDEX IF EXISTS planning_transcript_task_seq_idx;
CREATE INDEX IF NOT EXISTS transcript_task_seq_idx
  ON transcript (task_id, seq);

-- Enforces the actual idempotency guarantee: retrying an append with the
-- same (task_id, idempotency_key) must not double-post a message.
DROP INDEX IF EXISTS planning_transcript_idempotency_idx;
CREATE UNIQUE INDEX IF NOT EXISTS transcript_idempotency_idx
  ON transcript (task_id, idempotency_key);

-- 'question'/'answer' carry a JSON payload in `text`, not prose — the
-- AskUserQuestion MCP tool (fleet-core/internal/mcpserver) posts a
-- 'question' entry and long-polls for the matching 'answer' entry, which
-- the dashboard (not Discord) submits via AnswerQuestion. See docs/adr/0018.
--
-- 'system'/'assistant'/'user'/'result' are the SDK's own raw message
-- discriminants (reliability-findings.md #0's "relay everything, let the
-- UI decide" — worker/src/session.ts's logSdkMessage tags every message
-- with its own msg.type verbatim, not just assistant text as before).
-- 'tool_call' is PushToolTelemetry's sidecar-pushed summary — already
-- inserted before this change (coreserver/server.go), just never actually
-- listed here; a pre-existing gap this widening also closes.
--
-- 'permission_mode' is human-authored, from the dashboard's permission-mode
-- selector (docs/adr/0027) — `text` is the raw target SDK mode string.
--
-- 'permission_request'/'permission_response' are the sessions redesign's
-- generalized canUseTool prompt-and-wait pair (supersedes docs/adr/0021's
-- Write/Edit-absent-from-allowedTools gate): 'permission_request' is
-- worker-authored, posted by canUseTool for any tool call the SDK's
-- current permission mode would prompt for — `text` is a JSON
-- {tool, input} payload. 'permission_response' is the dashboard's
-- human-authored allow/deny decision, correlated back via reply_to_seq
-- (below) the same way 'answer' correlates to 'question'.
ALTER TABLE transcript DROP CONSTRAINT IF EXISTS planning_transcript_type_check;
ALTER TABLE transcript DROP CONSTRAINT IF EXISTS transcript_type_check;
ALTER TABLE transcript ADD CONSTRAINT transcript_type_check
  CHECK (type IN (
    'discussion', 'approve', 'abort', 'question', 'answer',
    'tool_call', 'system', 'assistant', 'user', 'result', 'permission_mode',
    'permission_request', 'permission_response'
  ) OR type IS NULL);

-- Retry/DLQ for the Discord-relay side effect only — the transcript entry
-- itself is already durable the moment the row above commits; these track
-- whether fleet-core has successfully posted it to the Discord thread yet,
-- so a transient Discord API failure retries instead of crashing the whole
-- watch loop (today's unguarded postReply can do exactly that).
ALTER TABLE transcript ADD COLUMN IF NOT EXISTS relayed_to_discord BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE transcript ADD COLUMN IF NOT EXISTS relay_attempts INT NOT NULL DEFAULT 0;
ALTER TABLE transcript ADD COLUMN IF NOT EXISTS relay_dead_letter BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE transcript ADD COLUMN IF NOT EXISTS relay_last_error TEXT;

-- reply_to_seq is the question-seq correlation reliability-findings.md #0
-- calls out as a real gap: today's "any pending question + any reply"
-- would let an unrelated message satisfy a blocked AskUserQuestion call.
-- NULL for every entry except an 'answer'/'permission_response' replying
-- to a specific 'question'/'permission_request' entry's own seq.
ALTER TABLE transcript ADD COLUMN IF NOT EXISTS reply_to_seq BIGINT;

-- Target-repo config, dashboard-editable (docs/adr/0028) — replaces the
-- hardcoded tasks.KnownRepos Go map so onboarding/editing a repo no longer
-- needs a core redeploy. No FK from tasks.repo: a removed repo shouldn't
-- retroactively break historical task rows.
CREATE TABLE IF NOT EXISTS repos (
  name        TEXT PRIMARY KEY,
  url         TEXT NOT NULL,
  base_branch TEXT NOT NULL DEFAULT '', -- '' means the provisioner defaults to "main"
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO repos (name, url, base_branch) VALUES
  ('dream-analyst', 'https://github.com/MohammadBnei/dream-analyst.git', ''),
  ('vos-monolith',  'https://github.com/MohammadBnei/vos-monolith.git', 'dev'),
  ('agent-fleet',   'https://github.com/MohammadBnei/agent-fleet.git', '')
ON CONFLICT (name) DO NOTHING;

-- Dashboard-editable, reusable guidance text (same no-redeploy pattern as
-- repos above) that an operator optionally attaches to a task at creation
-- time — replaces worker/src/session.ts's old unconditional, hardcoded
-- EXPLORE/REVIEW/INTERVIEW/PLAN/DOUBT/RECONCILE workflow. A task's base
-- prompt is just its own description; anything more is one of these,
-- picked per task, not forced on every task regardless of size.
CREATE TABLE IF NOT EXISTS prompt_snippets (
  id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  name       TEXT NOT NULL UNIQUE,
  text       TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Seeded with the content the old taskPrompt() used to force on every
-- task, split into the same stages, so nothing is lost — just made
-- optional and dashboard-editable rather than hardcoded and unconditional.
INSERT INTO prompt_snippets (name, text) VALUES
  ('Architecture interview',
   'If this task involves an architecturally non-trivial decision (new component, schema/protocol/API shape, a one-way door — see the architecture-interview skill''s own DOOR classification), type "/architecture-interview" as your next message to run it before you plan. Otherwise say in one line why it''s skipped and move on.'),
  ('Post a plan for approval',
   'Before implementing, post your plan via send_message (from="agent"), citing the specific files/paths you read and relied on. If Mohammad has put this session into plan mode (the dashboard''s mode picker), call ExitPlanMode with that same plan once you''re confident — it blocks for a real human decision the same way it does in Claude Code''s own plan mode. Outside plan mode, just proceed to implementing once you''ve posted the plan.'),
  ('Doubt-driven review',
   'Once you have a plan, if it involves a non-trivial decision per the doubt-driven-development skill''s own "When to Use" checklist, type "/doubt-driven-development" as your next message to run it. Otherwise say why it''s skipped and move on. Its cross-model escalation step is non-interactive in this environment — skip it and announce the skip.'),
  ('Open a PR when done',
   'Write the code following your plan, and add or update tests — run the repo''s test suite and make it pass; if there is no test suite, add one first. Update docs if relevant. Then commit your changes with git yourself via Bash, push, and open the PR yourself: push your branch (git push -u origin <your branch>), then gh pr create --title "..." --body "..." against this task''s base branch. Your git identity is already configured — just run the commands. Confirm the PR actually exists afterward (e.g. gh pr view) before telling Mohammad it''s ready.')
ON CONFLICT (name) DO NOTHING;

-- Resolved once at task-creation time (DashboardService.CreateTask joins
-- the operator's selected snippet texts) and stored alongside description
-- — same immutable-after-create, threaded-straight-through-at-redispatch
-- model description itself already uses. No FK/join table: a task's
-- guidance is a frozen snapshot, not a live reference to snippets that
-- might later be edited or deleted.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS guidance TEXT NOT NULL DEFAULT '';
