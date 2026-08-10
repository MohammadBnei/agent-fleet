-- Environment recipe system (docs/adr/0034): dashboard-editable, DB-backed
-- per-repo profiles, replacing provisioner/internal/k8s/names.go's
-- hardcoded StartCmdFor switch. A repo declares named profiles ("worker",
-- "e2e", "lint", ...) built from a bounded catalog of tool ingredients
-- (pod-local binaries) and service ingredients (postgres/redis, with a
-- scope mode controlling how they're shared across pods/tasks).
--
-- Three flat relational tables, not JSONB, matching this schema's existing
-- convention of TEXT CHECK (...) for bounded/enumerable fields (tasks.status
-- above) — JSONB is reserved for genuinely free-form payloads
-- (knowledge_journal.payload).

CREATE TABLE repo_profiles (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  repo_name   TEXT NOT NULL REFERENCES repos(name) ON DELETE CASCADE,
  name        TEXT NOT NULL,           -- 'worker' | 'e2e' | 'lint' | ...
  start_cmd   TEXT NOT NULL DEFAULT '',
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (repo_name, name)
);

CREATE TABLE repo_profile_tools (
  profile_id UUID NOT NULL REFERENCES repo_profiles(id) ON DELETE CASCADE,
  tool_key   TEXT NOT NULL CHECK (tool_key IN
              ('go-toolchain', 'bun-toolchain', 'golangci-lint', 'buf')),
  PRIMARY KEY (profile_id, tool_key)
);

-- scope_mode: 'pod-scoped' dies with the one pod that requested it;
-- 'task-scoped' lives in a shared per-repo instance with per-task
-- credentials minted on first use, reused by every pod belonging to the
-- same task; 'repo-scoped' is the same shared instance with no per-task
-- minting, every task hits the same database.
CREATE TABLE repo_profile_services (
  profile_id  UUID NOT NULL REFERENCES repo_profiles(id) ON DELETE CASCADE,
  service_key TEXT NOT NULL CHECK (service_key IN ('postgres', 'redis')),
  scope_mode  TEXT NOT NULL CHECK (scope_mode IN
              ('pod-scoped', 'task-scoped', 'repo-scoped')),
  PRIMARY KEY (profile_id, service_key)
);

-- Seed the real cases that motivated this system. dream-analyst's e2e
-- start_cmd is the actual fix for the two bugs that started this (ADR-0034):
-- explicit --host/--port bind (Vite never read $PORT), plus real
-- Postgres/Redis backing (dream-analyst/front is Prisma+ioredis-backed per
-- its own compose.yml).
WITH profiles AS (
  INSERT INTO repo_profiles (repo_name, name, start_cmd) VALUES
    ('dream-analyst', 'e2e',
     'cd front && bun install && bunx prisma migrate deploy && bunx vite dev --host 0.0.0.0 --port $PORT'),
    ('vos-monolith', 'e2e', 'bun install && bun run dev'),
    ('agent-fleet', 'lint', '')
  RETURNING id, repo_name, name
)
INSERT INTO repo_profile_tools (profile_id, tool_key)
SELECT id, 'bun-toolchain' FROM profiles WHERE repo_name = 'dream-analyst' AND name = 'e2e'
UNION ALL
SELECT id, 'bun-toolchain' FROM profiles WHERE repo_name = 'vos-monolith' AND name = 'e2e'
UNION ALL
SELECT id, k FROM profiles, unnest(ARRAY['go-toolchain', 'bun-toolchain', 'golangci-lint', 'buf']) AS k
  WHERE repo_name = 'agent-fleet' AND name = 'lint';

INSERT INTO repo_profile_services (profile_id, service_key, scope_mode)
SELECT id, 'postgres', 'task-scoped' FROM repo_profiles WHERE repo_name = 'dream-analyst' AND name = 'e2e'
UNION ALL
SELECT id, 'redis', 'task-scoped' FROM repo_profiles WHERE repo_name = 'dream-analyst' AND name = 'e2e';
