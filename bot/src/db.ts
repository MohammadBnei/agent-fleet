import pg from "pg";

const pool = new pg.Pool({
  host: process.env.AGENTFLEET_DB_HOST ?? "postgres.bnei.lan",
  port: Number(process.env.AGENTFLEET_DB_PORT ?? 5432),
  database: process.env.AGENTFLEET_DB_NAME ?? "agentfleetdb",
  user: process.env.AGENTFLEET_DB_USER ?? "dbuser_agentfleet",
  password: process.env.AGENTFLEET_DB_PASSWORD,
});

export const KNOWN_REPOS = ["dream-analyst", "vos-monolith"];

export async function createTask(
  repo: string,
  description: string,
  channelId: string,
  threadId: string,
  skipCritique = false,
): Promise<string> {
  const { rows } = await pool.query(
    `INSERT INTO tasks (repo, description, discord_channel_id, discord_thread_id, skip_critique)
     VALUES ($1, $2, $3, $4, $5) RETURNING id`,
    [repo, description, channelId, threadId, skipCritique],
  );
  return rows[0].id as string;
}

export async function findTaskIdByThread(threadId: string): Promise<string | null> {
  const { rows } = await pool.query(`SELECT id FROM tasks WHERE discord_thread_id = $1`, [
    threadId,
  ]);
  return rows[0]?.id ?? null;
}

// Writes directly to e2e_sessions rather than relaying through Redis like
// relayHumanMessage — a kill needs to work even if the worker's own
// session/transcript-watch loop is no longer running (today's /stop only
// works because something is actively polling Redis; an e2e session can
// outlive that). e2e-provisioner's own reconcile loop picks this up
// independently, on its own poll interval.
export async function requestE2eKill(taskId: string): Promise<boolean> {
  const { rows } = await pool.query(
    `UPDATE e2e_sessions SET kill_requested = true, updated_at = now()
     WHERE task_id = $1 AND status IN ('requested', 'running')
     RETURNING id`,
    [taskId],
  );
  return rows.length > 0;
}
