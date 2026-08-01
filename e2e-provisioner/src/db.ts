import pg from "pg";

// Same AGENTFLEET_DB_* env convention as worker/src/db.ts and bot/src/db.ts.
const pool = new pg.Pool({
  host: process.env.AGENTFLEET_DB_HOST ?? "postgres.bnei.lan",
  port: Number(process.env.AGENTFLEET_DB_PORT ?? 5432),
  database: process.env.AGENTFLEET_DB_NAME ?? "agentfleetdb",
  user: process.env.AGENTFLEET_DB_USER ?? "dbuser_agentfleet",
  password: process.env.AGENTFLEET_DB_PASSWORD,
});

export interface E2eSession {
  id: string;
  task_id: string;
  status: "requested" | "running" | "failed" | "torn_down";
  pod_name: string | null;
  ingress_path: string | null;
  kill_requested: boolean;
}

export interface TaskRef {
  id: string;
  repo: string;
  status: string;
}

export async function getTask(taskId: string): Promise<TaskRef | null> {
  const { rows } = await pool.query(`SELECT id, repo, status FROM tasks WHERE id = $1`, [taskId]);
  return rows[0] ?? null;
}

// The one active (not yet torn down) session for a task, if any — used both
// to avoid double-provisioning on a repeated request_e2e_env call and by
// mcpProxy to find which pod a task's Playwright tool calls should go to.
export async function getActiveSessionForTask(taskId: string): Promise<E2eSession | null> {
  const { rows } = await pool.query(
    `SELECT * FROM e2e_sessions WHERE task_id = $1 AND status IN ('requested', 'running')
     ORDER BY created_at DESC LIMIT 1`,
    [taskId],
  );
  return rows[0] ?? null;
}

export async function createSession(taskId: string): Promise<E2eSession> {
  const { rows } = await pool.query(
    `INSERT INTO e2e_sessions (task_id) VALUES ($1) RETURNING *`,
    [taskId],
  );
  return rows[0];
}

export async function setSessionStatus(
  id: string,
  status: E2eSession["status"],
  fields: Partial<Pick<E2eSession, "pod_name" | "ingress_path">> = {},
): Promise<void> {
  await pool.query(
    `UPDATE e2e_sessions
     SET status = $2, pod_name = COALESCE($3, pod_name), ingress_path = COALESCE($4, ingress_path), updated_at = now()
     WHERE id = $1`,
    [id, status, fields.pod_name ?? null, fields.ingress_path ?? null],
  );
}

export async function requestKill(taskId: string): Promise<void> {
  await pool.query(
    `UPDATE e2e_sessions SET kill_requested = true, updated_at = now()
     WHERE task_id = $1 AND status IN ('requested', 'running')`,
    [taskId],
  );
}

// Anything the reconcile loop (src/reconcile.ts) needs to tear down: the
// task reached a terminal status, or a kill (human via /e2e-kill, or the
// agent via the kill_env tool) was requested directly on the session.
export async function sessionsNeedingTeardown(): Promise<E2eSession[]> {
  const { rows } = await pool.query(
    `SELECT e2e_sessions.* FROM e2e_sessions
     JOIN tasks ON tasks.id = e2e_sessions.task_id
     WHERE e2e_sessions.status IN ('requested', 'running')
       AND (tasks.status IN ('done', 'failed', 'cancelled') OR e2e_sessions.kill_requested)`,
  );
  return rows;
}

export async function runningSessions(): Promise<E2eSession[]> {
  const { rows } = await pool.query(`SELECT * FROM e2e_sessions WHERE status IN ('requested', 'running')`);
  return rows;
}
