---
name: dashboard-e2e
description: Spin up the minimal local stack (throwaway Postgres + core + dashboard dev server) to Playwright-test the dashboard UI, then tear it all down cleanly. Use when the user wants to browser-test a dashboard change, verify a UI flow end-to-end locally, or write/run a Playwright check against the dashboard.
user-invocable: true
allowed-tools:
  - Read
  - Write
  - Edit
  - Bash(docker *)
  - Bash(go run *)
  - Bash(bun *)
  - Bash(bunx *)
  - Bash(curl *)
  - Bash(kill *)
  - Bash(pkill *)
  - Bash(lsof *)
  - Bash(rm *)
---

# /dashboard-e2e — minimal local stack for Playwright-testing the dashboard

No Playwright dependency exists anywhere in this repo (`dashboard/package.json`
has none) — this is ad hoc tooling via `bunx`, not a committed test suite.
`dashboard/vite.config.ts` already proxies `/api` to `http://localhost:8080`,
so the only two things needed to make the dashboard real locally are a
throwaway Postgres and a locally-running `core`. `core` needs no cluster or
Discord access: no `DISCORD_BOT_TOKEN` required (falls back to `noopNotifier`,
`core/cmd/core/run.go`).

**`PROVISIONER_GRPC_ADDR` can no longer point at nothing.** It used to be
harmless — creating a session was a row write, and only dispatch failed. Since
docs/adr/0048 the first *message* is what provisions: `PostMessage` calls
`WarmIfIdle` → `CreateWorkerPod` and **propagates the error**, so with no
reachable provisioner the message is never appended and the entire create flow
is untestable. Section 3a stands up a stub.

Every step below is disposable by construction (`--rm` container, background
PIDs, a scratch spec file) — section 6 tears every one of them back down.
**Always run section 6, even if an earlier step failed** — a stray Postgres
container or a process still holding port 8080/5173 will silently break the
next run of this skill.

## 1. Start a throwaway Postgres

`--rm` + a fixed container name makes cleanup one command. Port 5433 (not
5432) so it never collides with a real Postgres already running locally
(e.g. this machine's Pigsty stack).

```bash
docker run --rm -d --name agent-fleet-e2e-pg \
  -e POSTGRES_USER=agentfleet -e POSTGRES_PASSWORD=agentfleet -e POSTGRES_DB=agentfleetdb \
  -p 5433:5432 postgres:16

until docker exec agent-fleet-e2e-pg pg_isready -U agentfleet >/dev/null 2>&1; do sleep 0.5; done
```

## 2. Apply the schema

`db/migrations/` is the sole source of truth (docs/adr/0030) — `core`
itself no longer embeds or applies any schema. Run the same
`migrate/migrate` CLI image the real `migration/Dockerfile` wraps, pointed
at the Postgres container from step 1 (`host.docker.internal` — this
container needs to reach a port published on the Mac host, not another
container on the same Docker network).

```bash
export AGENTFLEET_DB_HOST=localhost AGENTFLEET_DB_PORT=5433 AGENTFLEET_DB_NAME=agentfleetdb \
       AGENTFLEET_DB_USER=agentfleet AGENTFLEET_DB_PASSWORD=agentfleet
docker run --rm -v "$(pwd)/db/migrations:/migrations" migrate/migrate:latest \
  -path=/migrations \
  -database "postgres://agentfleet:agentfleet@host.docker.internal:5433/agentfleetdb?sslmode=disable" \
  up
```

## 3a. Stub provisioner (background)

No Kubernetes involved — it records what it was asked to create and reports it
back. `ListWorkerPods` must echo those pods: returning an empty list lets
core's 60-second reconcile loop conclude every pod vanished and tear the
session down mid-run.

`E2E_SEEDED_SESSIONS` covers sessions inserted straight into Postgres with a
live `pod_phase` (see section 4a) — the same reconcile loop would otherwise
kill those too.

Write it to `/tmp`, never into the repo: it must never be mistaken for
something that can run in-cluster.

```bash
mkdir -p /tmp/agent-fleet-e2e-provisioner
# main.go: embed agentfleetv1.UnimplementedProvisionerServiceServer and
# implement three methods —
#   CreateWorkerPod   record sessionID -> a fake pod name, return it
#   TearDownSession   forget it
#   ListWorkerPods    return every recorded pod with Phase "Running"
# listening on 127.0.0.1:9091.
#
# go.mod: module e2estub, plus
#   replace github.com/MohammadBnei/agent-fleet/proto/gen/go => <repo>/proto/gen/go
# (GOWORK=off, or the repo's go.work fights the replace).

(cd /tmp/agent-fleet-e2e-provisioner && GOWORK=off go build -o stub . && ./stub) &
STUB_PID=$!
```

## 3. Run core (background)

Same DB env vars as above, still exported in this shell.

```bash
(cd core && PROVISIONER_GRPC_ADDR=127.0.0.1:9091 LOG_LEVEL=debug go run ./cmd/core) &
CORE_PID=$!
until curl -sf http://localhost:8080/healthz >/dev/null 2>&1; do sleep 0.5; done
```

Every `DashboardService` call needs the `X-Fleet-Dashboard` header
(`core/internal/dashboard/interceptor.go`); without it you get
`permission_denied: missing required header`. Handy for driving the API
directly from a spec:

```bash
curl -s -X POST http://localhost:8080/agentfleet.v1.DashboardService/ListSessions \
  -H 'content-type: application/json' -H 'X-Fleet-Dashboard: 1' -d '{}'
```

## 4. Run the dashboard dev server (background)

```bash
(cd dashboard && bun install && bun run dev) &
DASHBOARD_PID=$!
until curl -sf http://localhost:5173 >/dev/null 2>&1; do sleep 0.5; done
```

## 4a. Seed anything a spec cannot create through the UI

A blocked session is the main one: the UI has no way to make a pending
permission, so insert one. **Make the seed rerunnable and run it before every
suite** — answering a permission *consumes* it, so a second run against a
dirty database is a different test.

```sql
-- Delete by id first, and also delete whatever the last run created
-- (title LIKE 'e2e %', ...) — leftovers accumulate into the list every later
-- run reads, and a blocked session from run N looks seeded in run N+1.
INSERT INTO sessions (id, repo, title, pod_phase, activity_seen, seen_at,
                      last_active_at, last_entry_type, last_entry_from)
VALUES ('aaaaaaaa-0000-4000-8000-000000000001', 'agent-fleet', 'e2e inline decision',
        'POD_PHASE_RUNNING', true, now(), now(), 'permission_request', 'agent');

INSERT INTO transcript (session_id, seq, "from", text, type, idempotency_key)
VALUES ('aaaaaaaa-0000-4000-8000-000000000001', 1, 'agent',
        '{"tool":"Bash","input":{"command":"echo hi","description":"a seeded permission"}}',
        'permission_request', 'e2e-seed-inline');
```

`liveState` is derived, not stored: a live `pod_phase` plus an unanswered
`permission_request` is what makes a session `blocked`. `activity_seen` must be
true or the startup-stall sweep kills it after three minutes. Seeded UUIDs must
differ in their first 6 characters — the list renders `#{id.slice(0,6)}`. And
pass every seeded id to the stub's `E2E_SEEDED_SESSIONS`.

```bash
docker exec -i agent-fleet-e2e-pg psql -q -U agentfleet -d agentfleetdb < /tmp/seed.sql
```

## 5. Playwright-test it

`/tmp` has no `node_modules`, so a spec there cannot resolve
`@playwright/test`. Install into a scratch directory outside the repo and run
from there — same intent (nothing lands in git), but it actually runs.

```bash
PW=/tmp/agent-fleet-pw && mkdir -p "$PW" && cd "$PW"
bun add -d @playwright/test && bunx playwright install chromium
# write console.spec.ts here
bunx playwright test console.spec.ts --reporter=line --workers=1
```

Selector notes, each of which cost a debugging round:

- **Assert the dialog closed, not that the text appeared.** `getByText(marker)`
  also matches the textarea you just typed into, so it passes while the create
  is still in flight or has failed with the error rendered. The dialog closing
  is the only signal every RPC succeeded.
- **Poll `ListSessions`**; the dialog closing and your own fetch are
  independent.
- **Scope a decision click to its own card** (`div.relative` filtered by the
  session's label). Two blocked sessions mean two `allow` buttons, and an
  unscoped `.first()` answers whichever rendered first — leaving the next test
  nothing to answer.
- Mobile's list opens on its **needs-you** bucket, so a session another spec
  just unblocked is not on screen; click the `all` chip (`/^all \d+$/` — plain
  `/^all/` also matches `allow`).
- Vite's first dep-optimization pass can full-reload the page and abort an
  in-flight request. Load the page once before the suite.

For interactive exploration instead of a scripted assertion,
`bunx playwright codegen http://localhost:5173` records actions as you
click through the UI and emits a spec you can paste into the file above.

## 6. Teardown — run this every time, no exceptions

```bash
kill "$CORE_PID" "$DASHBOARD_PID" "$STUB_PID" 2>/dev/null
# in case the PIDs didn't stick (new shell, resumed session, etc.):
pkill -f "go run ./cmd/core" 2>/dev/null
pkill -f "vite" 2>/dev/null
pkill -f "agent-fleet-e2e-provisioner/stub" 2>/dev/null

docker stop agent-fleet-e2e-pg 2>/dev/null   # --rm above means stop also deletes it

rm -rf /tmp/agent-fleet-e2e-provisioner /tmp/agent-fleet-pw /tmp/seed.sql
rm -rf test-results playwright-report        # bunx playwright test's own output dirs, written to cwd
```

Verify nothing's left before ending the session:

```bash
docker ps --filter name=agent-fleet-e2e-pg   # expect empty (just the header row)
lsof -i :8080 -i :5173 -i :9091              # expect empty
```
