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
throwaway Postgres and a locally-running `core`. `core` needs zero cluster
or Discord access for this: no `DISCORD_BOT_TOKEN` required (falls back to
`noopNotifier`, `core/cmd/core/run.go`), and `PROVISIONER_GRPC_ADDR` can
point anywhere unreachable — task creation still works through the
dashboard UI; only actual worker-pod dispatch fails (logged, not fatal,
thanks to the fleet-wide `LOG_LEVEL` upgrade — those failures are now
actually visible instead of silent).

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

`core`'s own `migrate` subcommand embeds `db/schema.sql`
(`core/internal/db/embedded_schema.sql`, see `migrate.go`'s `go:embed`) —
no separate migration tool needed.

```bash
export AGENTFLEET_DB_HOST=localhost AGENTFLEET_DB_PORT=5433 AGENTFLEET_DB_NAME=agentfleetdb \
       AGENTFLEET_DB_USER=agentfleet AGENTFLEET_DB_PASSWORD=agentfleet
(cd core && go run ./cmd/core migrate)
```

## 3. Run core (background)

Same DB env vars as above, still exported in this shell.

```bash
(cd core && LOG_LEVEL=debug go run ./cmd/core) &
CORE_PID=$!
until curl -sf http://localhost:8080/healthz >/dev/null 2>&1; do sleep 0.5; done
```

## 4. Run the dashboard dev server (background)

```bash
(cd dashboard && bun install && bun run dev) &
DASHBOARD_PID=$!
until curl -sf http://localhost:5173 >/dev/null 2>&1; do sleep 0.5; done
```

## 5. Playwright-test it

Install ad hoc (caches globally after the first run — not a repo
dependency). Write any one-off spec to `/tmp`, never inside `dashboard/`,
so nothing new ends up staged in git. `playwright test` runs a standalone
spec file fine without a `playwright.config.ts`.

```bash
bunx playwright install --with-deps chromium   # first run only

cat > /tmp/dashboard-check.spec.ts <<'EOF'
import { test, expect } from "@playwright/test";

test("dashboard loads and lists tasks", async ({ page }) => {
  await page.goto("http://localhost:5173");
  await expect(page.locator("body")).toBeVisible();
  // ...assertions for the flow actually being verified
});
EOF

bunx playwright test /tmp/dashboard-check.spec.ts
```

For interactive exploration instead of a scripted assertion,
`bunx playwright codegen http://localhost:5173` records actions as you
click through the UI and emits a spec you can paste into the file above.

## 6. Teardown — run this every time, no exceptions

```bash
kill "$CORE_PID" "$DASHBOARD_PID" 2>/dev/null
# in case the PIDs didn't stick (new shell, resumed session, etc.):
pkill -f "go run ./cmd/core" 2>/dev/null
pkill -f "vite" 2>/dev/null

docker stop agent-fleet-e2e-pg 2>/dev/null   # --rm above means stop also deletes it

rm -f /tmp/dashboard-check.spec.ts
rm -rf test-results playwright-report        # bunx playwright test's own output dirs, written to cwd
```

Verify nothing's left before ending the session:

```bash
docker ps --filter name=agent-fleet-e2e-pg   # expect empty (just the header row)
lsof -i :8080 -i :5173                       # expect empty
```
