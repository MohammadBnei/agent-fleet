import pg from "pg";

const pool = new pg.Pool({
  host: process.env.AGENTFLEET_DB_HOST ?? "postgres.bnei.lan",
  port: Number(process.env.AGENTFLEET_DB_PORT ?? 5432),
  database: process.env.AGENTFLEET_DB_NAME ?? "agentfleetdb",
  user: process.env.AGENTFLEET_DB_USER ?? "dbuser_agentfleet",
  password: process.env.AGENTFLEET_DB_PASSWORD,
});

export interface Task {
  id: string;
  repo: string;
  description: string;
  status: string;
  discord_channel_id: string;
  discord_thread_id: string | null;
  claimed_by: string | null;
  pr_url: string | null;
  skip_critique: boolean;
}

// Atomic claim: SKIP LOCKED means two worker pods for the same repo (or a
// restarted pod racing its predecessor) never double-claim the same task.
export async function claimNextTask(repo: string, workerName: string): Promise<Task | null> {
  const { rows } = await pool.query(
    `UPDATE tasks SET status = 'claimed', claimed_by = $2, updated_at = now()
     WHERE id = (
       SELECT id FROM tasks
       WHERE repo = $1 AND status = 'pending'
       ORDER BY created_at
       FOR UPDATE SKIP LOCKED
       LIMIT 1
     )
     RETURNING *`,
    [repo, workerName],
  );
  return rows[0] ?? null;
}

export async function setTaskStatus(
  id: string,
  status: string,
  fields: Partial<Pick<Task, "pr_url">> = {},
): Promise<void> {
  await pool.query(
    `UPDATE tasks SET status = $2, pr_url = COALESCE($3, pr_url), updated_at = now() WHERE id = $1`,
    [id, status, fields.pr_url ?? null],
  );
}

export async function appendJournal(
  repo: string | null,
  actor: string,
  eventType: string,
  payload: Record<string, unknown> = {},
): Promise<void> {
  await pool.query(
    `INSERT INTO knowledge_journal (repo, actor, event_type, payload) VALUES ($1, $2, $3, $4)`,
    [repo, actor, eventType, JSON.stringify(payload)],
  );
}
